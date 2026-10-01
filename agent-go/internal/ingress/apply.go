package ingress

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// Runner executes one firewall binary with arguments (never a shell) and an
// optional stdin. Tests replace it with a fake kernel.
type Runner func(ctx context.Context, bin string, args []string, stdin string) ([]byte, error)

// RealRunner runs the host binaries.
func RealRunner(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

type binaries struct {
	family  Family
	tables  string
	restore string
}

func familyBinaries(f Family) binaries {
	if f == V6 {
		return binaries{family: V6, tables: "ip6tables", restore: "ip6tables-restore"}
	}
	return binaries{family: V4, tables: "iptables", restore: "iptables-restore"}
}

// Two equivalent forms of each jump. When the jump is not the first rule of
// its parent (ufw, firewalld or Docker inserted rules above it), the other
// form is inserted at position 1 first and the misplaced one is removed by
// its exact specification: there is never an instant without the jump, and
// no rule is ever deleted by number (which could hit someone else's rule).
func jumpForms(parent, chain string) [2][]string {
	return [2][]string{
		{"-j", chain},
		{"-m", "comment", "--comment", "impreza-ingress", "-j", chain},
	}
}

// Engine applies the host-wide rule set of one family.
type Engine struct {
	Run Runner
}

var errUnavailable = errors.New("firewall family unavailable")

func (e Engine) run(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
	return e.Run(ctx, bin, args, stdin)
}

func (e Engine) available(ctx context.Context, b binaries) bool {
	_, err := e.run(ctx, b.tables, []string{"-w", "5", "-S", InputChain}, "")
	return err == nil
}

func (e Engine) listChain(ctx context.Context, b binaries, chain string) ([]string, error) {
	out, err := e.run(ctx, b.tables, []string{"-w", "5", "-S", chain}, "")
	if err != nil {
		return nil, err
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func ruleLines(lines []string, chain string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "-A "+chain+" ") {
			out = append(out, l)
		}
	}
	return out
}

func matchesJump(line, parent string, form []string) bool {
	return line == "-A "+parent+" "+strings.Join(form, " ")
}

// ensureJump keeps exactly one jump to chain, as the first rule of parent.
// The form inserted at the top is always different from the one removed, so
// "-D <spec>" (which deletes the first match) can never remove the new top.
func (e Engine) ensureJump(ctx context.Context, b binaries, parent, chain string) error {
	lines, err := e.listChain(ctx, b, parent)
	if err != nil {
		return err
	}
	rules := ruleLines(lines, parent)
	forms := jumpForms(parent, chain)
	count := [2]int{}
	for _, l := range rules {
		for i, f := range forms {
			if matchesJump(l, parent, f) {
				count[i]++
			}
		}
	}
	if count[0]+count[1] == 1 && len(rules) > 0 &&
		(matchesJump(rules[0], parent, forms[0]) || matchesJump(rules[0], parent, forms[1])) {
		return nil
	}
	use, drop := 0, 1
	if count[0] > 0 && count[1] == 0 {
		use, drop = 1, 0
	}
	if count[use] > 0 {
		// Both forms present: clear the one to be inserted first (the other
		// form still jumps meanwhile, lower down).
		if err := e.deleteAll(ctx, b, parent, forms[use]); err != nil {
			return err
		}
	}
	if _, err := e.run(ctx, b.tables, append([]string{"-w", "5", "-I", parent, "1"}, forms[use]...), ""); err != nil {
		return err
	}
	return e.deleteAll(ctx, b, parent, forms[drop])
}

func (e Engine) deleteAll(ctx context.Context, b binaries, parent string, form []string) error {
	for i := 0; i < 16; i++ {
		if _, err := e.run(ctx, b.tables, append([]string{"-w", "5", "-C", parent}, form...), ""); err != nil {
			return nil
		}
		if _, err := e.run(ctx, b.tables, append([]string{"-w", "5", "-D", parent}, form...), ""); err != nil {
			return err
		}
	}
	return errors.New("too many ingress jumps in " + parent)
}

// Live is what one family has in the kernel after an apply.
type Live struct {
	Fingerprint string
	Rules       int
}

// applyFamily replaces both owned chains of one family in one transaction
// and puts the jumps first. The previous set stays live on any failure
// before the commit; a failure after it (a jump) is reported as such.
func (e Engine) applyFamily(ctx context.Context, f Family, policies []Policy, bridges []string) (Live, error) {
	b := familyBinaries(f)
	if !e.available(ctx, b) {
		return Live{}, errUnavailable
	}
	// DOCKER-USER is Docker's chain; it is only created when missing (boot
	// restore before docker.service, or ip6tables without Docker IPv6), and
	// never flushed.
	if _, err := e.listChain(ctx, b, DockerUserChain); err != nil {
		if _, err := e.run(ctx, b.tables, []string{"-w", "5", "-N", DockerUserChain}, ""); err != nil {
			return Live{}, errors.New("DOCKER-USER chain unavailable")
		}
	}
	forward, host := Rules(f, policies, bridges)
	if _, err := e.run(ctx, b.restore, []string{"-w", "5", "--noflush"}, restoreInput(forward, host)); err != nil {
		return Live{}, errors.New("iptables-restore rejected the ingress rule set")
	}
	if err := e.ensureJump(ctx, b, DockerUserChain, Chain); err != nil {
		return Live{}, errors.New("ingress jump could not be placed in DOCKER-USER")
	}
	if err := e.ensureJump(ctx, b, InputChain, HostChain); err != nil {
		return Live{}, errors.New("ingress jump could not be placed in INPUT")
	}
	return e.inspect(ctx, f, len(forward), len(host))
}

// inspect reads both owned chains and the jumps back and checks that the
// kernel holds the expected number of rules with the jumps first.
func (e Engine) inspect(ctx context.Context, f Family, wantForward, wantHost int) (Live, error) {
	b := familyBinaries(f)
	fwd, err := e.listChain(ctx, b, Chain)
	if err != nil {
		return Live{}, errors.New("ingress chain missing after apply")
	}
	host, err := e.listChain(ctx, b, HostChain)
	if err != nil {
		return Live{}, errors.New("ingress host chain missing after apply")
	}
	fr, hr := ruleLines(fwd, Chain), ruleLines(host, HostChain)
	if wantForward >= 0 && (len(fr) != wantForward || len(hr) != wantHost) {
		return Live{}, errors.New("ingress chains do not hold the expected rules")
	}
	for _, j := range [][2]string{{DockerUserChain, Chain}, {InputChain, HostChain}} {
		lines, err := e.listChain(ctx, b, j[0])
		if err != nil {
			return Live{}, errors.New("ingress parent chain unreadable")
		}
		rules := ruleLines(lines, j[0])
		forms := jumpForms(j[0], j[1])
		if len(rules) == 0 || !(matchesJump(rules[0], j[0], forms[0]) || matchesJump(rules[0], j[0], forms[1])) {
			return Live{}, errors.New("ingress jump is not the first rule of " + j[0])
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(fr, "\n") + "\n--\n" + strings.Join(hr, "\n")))
	return Live{Fingerprint: string(f) + ":" + hex.EncodeToString(sum[:]), Rules: len(fr) + len(hr)}, nil
}

// removeFamily deletes both jumps and both owned chains. Absent pieces are
// not an error; nothing else is touched.
func (e Engine) removeFamily(ctx context.Context, f Family) error {
	b := familyBinaries(f)
	if !e.available(ctx, b) {
		return nil
	}
	var firstErr error
	for _, j := range [][2]string{{DockerUserChain, Chain}, {InputChain, HostChain}} {
		if _, err := e.listChain(ctx, b, j[0]); err != nil {
			continue
		}
		for _, form := range jumpForms(j[0], j[1]) {
			if err := e.deleteAll(ctx, b, j[0], form); err != nil {
				firstErr = errors.New("ingress jump could not be removed from " + j[0])
			}
		}
	}
	for _, c := range []string{Chain, HostChain} {
		if _, err := e.listChain(ctx, b, c); err != nil {
			continue
		}
		if _, err := e.run(ctx, b.tables, []string{"-w", "5", "-F", c}, ""); err != nil && firstErr == nil {
			firstErr = errors.New("ingress chain could not be flushed")
		}
		if _, err := e.run(ctx, b.tables, []string{"-w", "5", "-X", c}, ""); err != nil && firstErr == nil {
			firstErr = errors.New("ingress chain could not be deleted")
		}
	}
	return firstErr
}

// hostHasGlobalIPv6 reports whether the host has a routable IPv6 address:
// without ip6tables such a host cannot be restricted, and that is a failure,
// not a silent pass.
func hostHasGlobalIPv6() bool {
	raw, err := os.ReadFile("/proc/net/if_inet6")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[5] == "lo" {
			continue
		}
		// scope 00 is global
		if fields[3] == "00" {
			return true
		}
	}
	return false
}
