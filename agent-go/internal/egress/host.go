package egress

// Host INPUT half of the baseline. A tenant container must not reach
// a host-local service nobody opened, but the agent never overrides a
// decision of the operator's firewall:
//
//   - The jump to IMPREZA-EGRESS-HOST is the LAST rule of INPUT, not the
//     first. Whatever the operator's rules (UFW, iptables-persistent, any
//     tool that writes INPUT) accept from a Docker bridge is accepted before
//     the baseline is consulted; the baseline only decides what nobody
//     decided. The reconcile keeps the jump last, in one iptables-restore
//     transaction (delete and append commit together, no gap).
//   - The ports the host publishes through Docker (the proxy on 80/443,
//     apps in host_ports) are returned: a container reaching them through
//     the host's own address (the WordPress loopback, WP-Cron, Site Health)
//     gets exactly what the internet gets.
//   - The operator can add persistent exceptions or turn the half off for
//     the whole server (impreza-agent egress host ...). The policy lives in
//     <StateDir>/egress-host-policy.json and survives reboot and reconcile.
//   - Every apply that changes something, and the first one of each agent
//     process, is reported through Notify (the agent writes one journal
//     line): counts and a fingerprint, never an address.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Notify, when set by the agent, receives one line per host apply that
// changed the firewall and for the first apply of each process.
var Notify func(msg string, args ...any)

const (
	hostPolicyFile   = "egress-host-policy.json"
	maxHostExcepts   = 256
	publishedTimeout = 5 * time.Second
)

// HostPolicy is the operator's persistent choice for the host INPUT half.
type HostPolicy struct {
	Version  int      `json:"version"`
	Disabled bool     `json:"disabled"`
	Allow    []string `json:"allow"` // "tcp/8080", "udp/53"
}

var portProto = regexp.MustCompile(`^(tcp|udp)/([0-9]{1,5})$`)

// NormalizeHostPort validates "proto/port" (tcp or udp, 1-65535).
func NormalizeHostPort(s string) (string, error) {
	m := portProto.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return "", errors.New("use tcp/PORT or udp/PORT, for example tcp/8080")
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("the port must be between 1 and 65535")
	}
	return m[1] + "/" + strconv.Itoa(n), nil
}

func normalizeList(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		n, err := NormalizeHostPort(s)
		if err != nil {
			return nil, err
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sortPorts(out)
	if len(out) > maxHostExcepts {
		return nil, fmt.Errorf("at most %d host exceptions", maxHostExcepts)
	}
	return out, nil
}

func sortPorts(p []string) {
	sort.Slice(p, func(i, j int) bool {
		ai, aj := strings.SplitN(p[i], "/", 2), strings.SplitN(p[j], "/", 2)
		ni, _ := strconv.Atoi(ai[1])
		nj, _ := strconv.Atoi(aj[1])
		if ni != nj {
			return ni < nj
		}
		return ai[0] < aj[0]
	})
}

func hostPolicyPath(stateDir string) (string, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return "", errors.New("invalid egress state directory")
	}
	return filepath.Join(stateDir, hostPolicyFile), nil
}

// LoadHostPolicy reads the operator policy; a missing file is the default
// (enabled, no exceptions). An unreadable or invalid file is an error: the
// caller keeps the rules already in place rather than guess.
func LoadHostPolicy(stateDir string) (HostPolicy, error) {
	p := HostPolicy{Version: 1}
	path, err := hostPolicyPath(stateDir)
	if err != nil {
		return p, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	raw, err := readBoundedRegularFile(path, 16384)
	if err != nil {
		return p, errors.New("egress host policy unreadable")
	}
	if json.Unmarshal(raw, &p) != nil || p.Version != 1 {
		return HostPolicy{Version: 1}, errors.New("egress host policy is invalid")
	}
	if p.Allow, err = normalizeList(p.Allow); err != nil {
		return HostPolicy{Version: 1}, errors.New("egress host policy is invalid: " + err.Error())
	}
	return p, nil
}

// SaveHostPolicy validates and writes the policy atomically, mode 0600.
func SaveHostPolicy(stateDir string, p HostPolicy) error {
	path, err := hostPolicyPath(stateDir)
	if err != nil {
		return err
	}
	p.Version = 1
	if p.Allow, err = normalizeList(p.Allow); err != nil {
		return err
	}
	if p.Allow == nil {
		p.Allow = []string{}
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("egress host policy path is not a regular file")
	}
	raw, _ := json.MarshalIndent(p, "", "  ")
	tmp, err := os.CreateTemp(stateDir, ".egress-host-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// dockerRunner runs the docker CLI; tests replace it.
type dockerRunner func(ctx context.Context, args ...string) ([]byte, error)

func realDocker(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, errors.New("docker unavailable")
	}
	if len(out) > 4<<20 {
		return nil, errors.New("docker output too large")
	}
	return out, nil
}

// PublishedPorts lists the ports Docker publishes on a non-loopback host
// address, as "proto/port", within publishedTimeout. Loopback-only bindings
// are not reachable from a bridge anyway and are left out.
func publishedPorts(ctx context.Context, docker dockerRunner) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, publishedTimeout)
	defer cancel()
	out, err := docker(ctx, "ps", "-q", "--no-trunc")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return []string{}, nil
	}
	if len(ids) > 1000 {
		return nil, errors.New("too many containers")
	}
	out, err = docker(ctx, append([]string{"inspect", "--format", "{{json .NetworkSettings.Ports}}"}, ids...)...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ports []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var m map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		for key, binds := range m {
			parts := strings.SplitN(key, "/", 2)
			if len(parts) != 2 || (parts[1] != "tcp" && parts[1] != "udp") {
				continue
			}
			for _, b := range binds {
				if strings.HasPrefix(b.HostIP, "127.") || b.HostIP == "::1" {
					continue
				}
				n, err := NormalizeHostPort(parts[1] + "/" + b.HostPort)
				if err == nil && !seen[n] {
					seen[n] = true
					ports = append(ports, n)
				}
			}
		}
	}
	sortPorts(ports)
	if ports == nil {
		ports = []string{}
	}
	return ports, nil
}

// HostRulesFor renders the host chain for one family: per Docker bridge
// name, established and ICMP return, then each exception, then DROP.
func HostRulesFor(icmp string, exceptions []string) [][]string {
	var rules [][]string
	for _, iface := range []string{"docker0", "br+"} {
		rules = append(rules,
			[]string{"-i", iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
			[]string{"-i", iface, "-p", icmp, "-j", "RETURN"},
		)
		for _, e := range exceptions {
			pp := strings.SplitN(e, "/", 2)
			rules = append(rules, []string{"-i", iface, "-p", pp[0], "-m", pp[0], "--dport", pp[1], "-j", "RETURN"})
		}
		rules = append(rules, []string{"-i", iface, "-j", "DROP"})
	}
	return rules
}

// restoreRunner feeds one iptables-restore --noflush transaction.
type restoreRunner func(ctx context.Context, input string) error

func realRestoreFor(bin string) restoreRunner {
	return func(ctx context.Context, input string) error {
		cmd := exec.CommandContext(ctx, bin, "-w", "5", "--noflush")
		cmd.Stdin = strings.NewReader(input)
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = out
			return errors.New(bin + " rejected the egress host transaction")
		}
		return nil
	}
}

type hostFamily struct {
	label   string
	icmp    string
	tables  commandRunner
	restore restoreRunner
	read    func(string) Status
	write   func(string, Status) error
}

var announced sync.Map // label -> true after the first report of the process

// inputJumps returns how many rules of INPUT jump to the host chain and
// whether the only one is the last rule.
func inputJumps(input []byte) (count int, last bool) {
	var rules []string
	for _, l := range strings.Split(string(input), "\n") {
		if strings.HasPrefix(l, "-A INPUT ") {
			rules = append(rules, strings.TrimSpace(l))
		}
	}
	jump := "-A INPUT -j " + HostChain
	for _, r := range rules {
		if r == jump {
			count++
		}
	}
	return count, count == 1 && rules[len(rules)-1] == jump
}

func reconcileHost(ctx context.Context, fam hostFamily, stateDir string, docker dockerRunner) (Status, error) {
	prev := fam.read(stateDir)
	policy, perr := LoadHostPolicy(stateDir)
	if perr != nil {
		// Keep what is in place: never guess the operator's intent.
		prev.Error = perr.Error()
		return prev, perr
	}
	published, derr := publishedPorts(ctx, docker)
	if derr != nil {
		published = prev.Ports // Docker not answering: the last list applied
	}
	if published == nil {
		published = []string{}
	}
	input, err := fam.tables(ctx, "-S", "INPUT")
	if err != nil {
		return Status{Ports: published}, errors.New(fam.label + " parent chain unavailable")
	}
	jumps, last := inputJumps(input)
	current, cerr := fam.tables(ctx, "-S", HostChain)

	if policy.Disabled {
		status := Status{Disabled: true, Ports: published, Operator: len(policy.Allow)}
		if jumps == 0 && cerr != nil {
			fam.report(stateDir, prev, status, false)
			return status, nil
		}
		// Chain declaration first, then the jumps out, then the chain: one
		// transaction, nothing of ours left.
		var b strings.Builder
		b.WriteString("*filter\n:" + HostChain + " - [0:0]\n")
		for i := 0; i < jumps; i++ {
			b.WriteString("-D INPUT -j " + HostChain + "\n")
		}
		b.WriteString("-X " + HostChain + "\nCOMMIT\n")
		if err := fam.restore(ctx, b.String()); err != nil {
			return prev, errors.New(fam.label + " chain cannot be removed")
		}
		fam.report(stateDir, prev, status, true)
		return status, nil
	}

	exceptions, err := normalizeList(append(append([]string{}, published...), policy.Allow...))
	if err != nil {
		return prev, errors.New(fam.label + ": " + err.Error())
	}
	wanted := HostRulesFor(fam.icmp, exceptions)
	status := Status{Rules: len(wanted), Wanted: fingerprint(wanted), Ports: published, Operator: len(policy.Allow), Position: "last"}
	if cerr == nil && last && prev.Applied && prev.Wanted == status.Wanted && prev.Fingerprint == sha256Sum(current) {
		status.Applied, status.Fingerprint = true, prev.Fingerprint
		fam.report(stateDir, prev, status, false)
		return status, nil
	}
	var b strings.Builder
	b.WriteString("*filter\n:" + HostChain + " - [0:0]\n")
	for _, r := range wanted {
		b.WriteString("-A " + HostChain + " " + strings.Join(r, " ") + "\n")
	}
	for i := 0; i < jumps; i++ {
		b.WriteString("-D INPUT -j " + HostChain + "\n")
	}
	b.WriteString("-A INPUT -j " + HostChain + "\nCOMMIT\n")
	if err := fam.restore(ctx, b.String()); err != nil {
		return Status{Ports: published}, errors.New(fam.label + " baseline cannot be applied")
	}
	current, err = fam.tables(ctx, "-S", HostChain)
	if err != nil {
		return Status{Ports: published}, errors.New(fam.label + " chain cannot be verified after apply")
	}
	input, err = fam.tables(ctx, "-S", "INPUT")
	if n, isLast := inputJumps(input); err != nil || n != 1 || !isLast {
		return Status{Ports: published}, errors.New(fam.label + " jump is not the last INPUT rule after apply")
	}
	status.Applied, status.Fingerprint = true, sha256Sum(current)
	fam.report(stateDir, prev, status, true)
	return status, nil
}

// report writes the one journal line: when the firewall changed, and once
// per process. Counts and a short fingerprint only.
func (fam hostFamily) report(_ string, prev, now Status, changed bool) {
	_, seen := announced.LoadOrStore(fam.label, true)
	if Notify == nil || (!changed && seen) {
		return
	}
	fp := now.Fingerprint
	if len(fp) > 12 {
		fp = fp[:12]
	}
	if now.Disabled {
		Notify("egress host baseline disabled by the operator", "family", fam.label, "changed", changed)
		return
	}
	Notify("egress host baseline applied", "family", fam.label, "changed", changed, "position", "last INPUT rule",
		"rules", now.Rules, "published_ports", len(now.Ports), "operator_exceptions", now.Operator, "fingerprint", fp)
}

func applyHostWith(ctx context.Context, fam hostFamily, stateDir string, docker dockerRunner) error {
	status, err := reconcileHost(ctx, fam, stateDir, docker)
	if err != nil {
		status.Applied = false
		status.Error = err.Error()
	}
	status.LastAttempt = nowRFC3339()
	if writeErr := fam.write(stateDir, status); writeErr != nil && err == nil {
		return errors.New(fam.label + " status could not be recorded")
	}
	return err
}

func hostFamily4(run commandRunner, restore restoreRunner) hostFamily {
	return hostFamily{label: "egress host", icmp: "icmp", tables: run, restore: restore, read: readHostStatus4, write: writeHostStatus4}
}

func hostFamily6(run commandRunner, restore restoreRunner) hostFamily {
	return hostFamily{label: "egress v6 host", icmp: "ipv6-icmp", tables: run, restore: restore, read: readHostStatus6, write: writeHostStatus6}
}

// HostStatus returns the recorded state of the host half, v4 and v6.
func HostStatus(stateDir string) (Status, Status) {
	return readHostStatus4(stateDir), readHostStatus6(stateDir)
}
