package egress

// IPv6 half of the baseline. The IPv4 contract in egress.go applies here
// unchanged in spirit; the differences that matter:
//
//   - The tool is ip6tables. Docker creates DOCKER-USER in both families
//     when ip6tables is enabled; on hosts without IPv6 or without the
//     ip6tables binary the whole v6 apply is skipped and recorded as
//     unavailable — it is never an error that stops the agent (fail-open,
//     same as v4), but it IS recorded so an operator can see the gap.
//   - Blocked destinations: link-local metadata (fe80::/10), the unique
//     local range (fc00::/7) and the IPv4-mapped metadata space that some
//     environments synthesize (::ffff:169.254.0.0/112). Site-local was
//     deprecated by RFC 3879 and is not separately listed.
//   - DNS to the host's own IPv6 resolvers is excepted before the blocks,
//     mirroring v4, so hosts whose v6 resolver is ULA keep working.
//   - SMTP and the per-source rate limit apply with the same parameters.
//   - Interface scope: the v4 baseline trusts Docker's default bridge
//     names; for v6 the same names are used, plus the RETURN for
//     same-bridge frames. Custom bridges are covered by the physdev
//     RETURN at the top: frames that never leave a bridge are internal
//     and return; frames that do leave hit the blocked list regardless
//     of which bridge originated them. That is the bridge-agnostic half;
//     the physdev rule was already in the v4 chain for the same reason —
//     here it is the ONLY bridge rule, so custom networks are filtered
//     by destination rather than by interface name.

import (
	"context"
	"errors"
	"os/exec"
)

// Rules6 returns the exact v6 chain content, in order.
func Rules6(resolvers6 []string) [][]string {
	rules := [][]string{
		{"-m", "physdev", "!", "--physdev-in", "+", "-j", "RETURN"},
		{"-m", "physdev", "--physdev-is-bridged", "-j", "RETURN"},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
	}
	for _, r := range resolvers6 {
		rules = append(rules,
			[]string{"-p", "udp", "-d", r + "/128", "-m", "udp", "--dport", "53", "-j", "RETURN"},
			[]string{"-p", "tcp", "-d", r + "/128", "-m", "tcp", "--dport", "53", "-j", "RETURN"},
		)
	}
	blocked := [][]string{
		{"-d", "fe80::/10", "-j", "DROP"},
		{"-d", "fc00::/7", "-j", "DROP"},
		{"-d", "::ffff:169.254.0.0/112", "-j", "DROP"},
		{"-p", "tcp", "-m", "conntrack", "--ctstate", "NEW", "-m", "multiport", "--dports", "25,465,587", "-j", "DROP"},
		{"-m", "conntrack", "--ctstate", "NEW", "-m", "hashlimit", "--hashlimit-above", rateLimit, "--hashlimit-burst", rateBurst, "--hashlimit-mode", "srcip", "--hashlimit-name", limitTable, "-j", "DROP"},
	}
	rules = append(rules, blocked...)
	return append(rules, []string{"-j", "RETURN"})
}

// ReadResolvers6 is ReadResolvers for IPv6 nameservers: only Is6() hosts,
// deduplicated, in file order, capped at the same 8.
func ReadResolvers6(path string) ([]string, error) {
	base, err := ReadResolversAny(path)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, a := range base {
		if !a.Is6() || seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		out = append(out, a.String())
	}
	// No IPv6 resolver configured is normal on v4-only hosts; the v6 apply
	// still runs (blocks without DNS exceptions) so a container cannot use
	// "no v6 resolver" as a signal to skip the firewall.
	return out, nil
}

func realRunner6(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ip6tables", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.New("ip6tables rejected the egress baseline operation")
	}
	return out, nil
}

// ip6tablesAvailable reports whether the binary exists; a missing binary or
// a kernel without ip6tables support is an unavailable, not an error.
func ip6tablesAvailable() bool {
	out, err := exec.Command("sh", "-c", "command -v ip6tables >/dev/null 2>&1 && ip6tables -L DOCKER-USER >/dev/null 2>&1").CombinedOutput()
	_ = out
	return err == nil
}

// apply6 mirrors apply() for the v6 family. The status shares the same file
// with a v6 section so operators read one document.
func apply6(ctx context.Context, run commandRunner, stateDir string, resolvers6 []string) (Status, error) {
	return reconcileChain(ctx, run, Chain, ParentChain, "egress v6", Rules6(resolvers6), len(resolvers6), readStatus6(stateDir))
}

// HostRules6 returns the exact v6 INPUT chain content. Same scoping as v4;
// ICMPv6 carries NDP/RS in v6 and must stay open for the bridge to work.
func HostRules6() [][]string {
	var rules [][]string
	for _, iface := range []string{"docker0", "br+"} {
		rules = append(rules,
			[]string{"-i", iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
			[]string{"-i", iface, "-p", "ipv6-icmp", "-j", "RETURN"},
			[]string{"-i", iface, "-j", "DROP"},
		)
	}
	return rules
}

func applyHost6(ctx context.Context, run commandRunner, stateDir string) (Status, error) {
	return reconcileChain(ctx, run, HostChain, "INPUT", "egress v6 host", HostRules6(), 0, readHostStatus6(stateDir))
}

// Apply6 installs or verifies the v6 baseline (FORWARD and host INPUT).
// Unavailable ip6tables is recorded and returns nil (not an error): the v4
// baseline governs and the status documents the v6 gap.
func Apply6(ctx context.Context, stateDir string) error {
	if !ip6tablesAvailable() {
		unavailable := Status{Applied: false, Error: "ip6tables or DOCKER-USER (v6) unavailable on this host"}
		errF := recordStatus6(stateDir, unavailable)
		errH := recordHostStatus6(stateDir, unavailable)
		return errors.Join(errF, errH)
	}
	errForward := applyAll6(ctx, realRunner6, "/etc/resolv.conf", stateDir)
	errHost := applyHostAll6(ctx, realRunner6, stateDir)
	return errors.Join(errForward, errHost)
}

func applyAll6(ctx context.Context, run commandRunner, resolvPath, stateDir string) error {
	resolvers6, _ := ReadResolvers6(resolvPath)
	status, err := apply6(ctx, run, stateDir, resolvers6)
	if err != nil {
		status.Applied = false
		status.Error = err.Error()
	}
	status.LastAttempt = nowRFC3339()
	if writeErr := writeStatus6(stateDir, status); writeErr != nil && err == nil {
		return errors.New("egress v6 status could not be recorded")
	}
	return err
}

func applyHostAll6(ctx context.Context, run commandRunner, stateDir string) error {
	status, err := applyHost6(ctx, run, stateDir)
	if err != nil {
		status.Applied = false
		status.Error = err.Error()
	}
	status.LastAttempt = nowRFC3339()
	if writeErr := writeHostStatus6(stateDir, status); writeErr != nil && err == nil {
		return errors.New("egress v6 host status could not be recorded")
	}
	return err
}

func recordStatus6(stateDir string, s Status) error {
	s.LastAttempt = nowRFC3339()
	return writeStatus6(stateDir, s)
}

func recordHostStatus6(stateDir string, s Status) error {
	s.LastAttempt = nowRFC3339()
	return writeHostStatus6(stateDir, s)
}
