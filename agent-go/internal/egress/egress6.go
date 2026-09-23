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
//   - Only traffic received from a bridge port is filtered. Non-bridge
//     ingress and traffic staying on one bridge retain Docker's policy.

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Rules6 filters only bridge-originated forwarding, including custom bridge names.
// Inbound traffic to published container ports must retain Docker's policy.
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

// apply6 mirrors apply() for the v6 family. The status shares the same file
// with a v6 section so operators read one document.
func apply6(ctx context.Context, run commandRunner, stateDir string, resolvers6 []string) (Status, error) {
	wanted := Rules6(resolvers6)
	status := Status{Rules: len(wanted), Resolvers: len(resolvers6), Wanted: fingerprint(wanted)}
	previous := readStatus6(stateDir)
	current, err := run(ctx, "-S", Chain)
	if err != nil {
		if _, err = run(ctx, "-N", Chain); err != nil {
			return status, errors.New("egress v6 chain cannot be created")
		}
		current = nil
	}
	parent, err := run(ctx, "-S", ParentChain)
	if err != nil {
		return status, errors.New("docker v6 user chain unavailable")
	}
	linked := strings.Contains(string(parent), "-A "+ParentChain+" -j "+Chain+"\n") || strings.HasSuffix(strings.TrimRight(string(parent), "\n"), "-A "+ParentChain+" -j "+Chain)
	h := sha256Sum(current)
	if linked && len(current) != 0 && previous.Applied && previous.Wanted == status.Wanted && previous.Fingerprint == h {
		status.Applied = true
		status.Fingerprint = h
		return status, nil
	}
	if len(current) != 0 {
		if _, err = run(ctx, "-F", Chain); err != nil {
			return status, errors.New("egress v6 chain cannot be reconciled")
		}
	}
	for _, rule := range wanted {
		if _, err = run(ctx, append([]string{"-A", Chain}, rule...)...); err != nil {
			return status, errors.New("egress v6 baseline rule cannot be installed")
		}
	}
	current, err = run(ctx, "-S", Chain)
	if err != nil {
		return status, errors.New("egress v6 chain cannot be verified after apply")
	}
	status.Fingerprint = sha256Sum(current)
	if !linked {
		if _, err = run(ctx, "-I", ParentChain, "1", "-j", Chain); err != nil {
			return status, errors.New("egress v6 chain cannot be linked into the docker forwarding path")
		}
	}
	status.Applied = true
	return status, nil
}

// Apply6 installs or verifies the v6 baseline. Unavailable ip6tables is
// recorded and returned as an advisory error; the caller keeps the agent running.
func Apply6(ctx context.Context, stateDir string) error {
	if _, err := realRunner6(ctx, "-S", ParentChain); err != nil {
		statusErr := recordStatus6(stateDir, Status{Applied: false, Error: "ip6tables or DOCKER-USER (v6) unavailable on this host"})
		if statusErr != nil {
			return statusErr
		}
		return errors.New("IPv6 baseline unavailable; recorded without stopping the agent")
	}
	return applyAll6(ctx, realRunner6, "/etc/resolv.conf", stateDir)
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

func recordStatus6(stateDir string, s Status) error {
	s.LastAttempt = nowRFC3339()
	return writeStatus6(stateDir, s)
}
