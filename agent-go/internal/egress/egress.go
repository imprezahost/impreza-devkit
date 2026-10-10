// Package egress applies the host egress baseline for tenant containers.
//
// Security contract (S4 of the private hardening plan): an anonymous tenant
// container must not turn the host's public IPs into a spam, scan or C2
// source, and must not reach host-local services or link-local metadata. The
// agent manages chains of its own: IMPREZA-EGRESS, linked at the top of
// DOCKER-USER (the chain Docker documents for operator rules and never
// flushes), and IMPREZA-EGRESS-HOST, linked at the top of INPUT. Only the
// content of our own chains is ever reconciled; DOCKER-USER, INPUT and every
// other chain are never flushed or reordered by us. Every chain ends in
// RETURN (or matches nothing): this is a baseline, never a default-deny.
//
// Scope and known limits:
//   - Both families: IPv4 (iptables) and IPv6 (ip6tables, egress6.go).
//   - Rule precedence inside the FORWARD chain: same-bridge frames, then the
//     link-local metadata range (interface-agnostic, so custom-named bridges
//     and macvlan-style tenant paths cannot reach 169.254.0.0/16 either),
//     then established flows, then DNS to the host's own resolvers, then
//     RFC1918 destinations, outbound SMTP (25/465/587) and a per-source-IP
//     rate limit on new flows. DNS is explicitly allowed to the resolvers
//     configured in /etc/resolv.conf so hosts whose resolver is RFC1918 (a
//     LAN gateway) do not break; a stub resolver (127.0.0.53) never traverses
//     FORWARD and the exception is simply inert there. DNS to hardcoded
//     public resolvers is deliberately not blocked in this phase; DNS-based
//     exfiltration is part of the edge/netflow backlog, not of this baseline.
//   - The host INPUT chain only matches packets entering from Docker's
//     bridge interfaces: established/related and ICMP are returned to the
//     operator's INPUT policy, every other flow from a bridge to a
//     host-local service is dropped. Management traffic arrives on the
//     uplink and never matches, so the chain cannot lock an operator out.
//     Docker's port publishing is DNAT'ed in PREROUTING and reaches
//     containers through FORWARD, not through these INPUT rules.
//   - RFC1918 drops stay scoped to the default bridge names (docker0/br+):
//     hosts that route between local networks keep working. The metadata
//     drop is the one deliberately interface-agnostic destination rule.
//   - Application failure is fail-open by design: the agent keeps running and
//     records the state in <StateDir>/egress.json. An egress firewall must
//     never take deploys down; the residual risk window is between Docker
//     start and agent start, and between a Docker daemon restart that drops
//     our link and the next reconcile. The run loop re-applies this baseline
//     periodically (cmd/run.go) to close that second window.
//   - Legitimate outbound mail will require an explicit future opt-in; the
//     rate limit (300 new flows/minute sustained, burst 600, per source IP)
//     is sized so image pulls and package installs are unaffected while
//     scanning or beaconing patterns trip it.
package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Chain is the only FORWARD chain whose content the agent ever reconciles.
	Chain = "IMPREZA-EGRESS"
	// HostChain is the agent-owned INPUT chain. It only matches ingress from
	// Docker bridge interfaces; see the package doc for the lockout analysis.
	HostChain = "IMPREZA-EGRESS-HOST"
	// ParentChain is the Docker-documented operator chain at the top of
	// FORWARD. Docker creates it and never flushes user content from it.
	ParentChain = "DOCKER-USER"
	// New flows per minute sustained per container source IP, with the burst
	// bucket below. Sized so dependency downloads (many parallel but
	// established connections) never trip it; scanning/C2 churn does.
	rateLimit  = "300/minute"
	rateBurst  = "600"
	limitTable = "impreza_egress"
)

const resolvConfLimit = 64 * 1024

// Status is the durable, operator-readable state of the last baseline apply.
type Status struct {
	Applied     bool   `json:"applied"`
	Error       string `json:"error,omitempty"`
	LastAttempt string `json:"last_attempt"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Wanted      string `json:"wanted,omitempty"`
	Rules       int    `json:"rules"`
	Resolvers   int    `json:"resolvers"`
	// Host INPUT half only: where the jump sits, the published ports
	// returned (ports, never addresses), how many operator exceptions, and
	// whether the operator turned the half off.
	Position string   `json:"position,omitempty"`
	Ports    []string `json:"ports,omitempty"`
	// The Docker bridge interfaces the per-iface rules were scoped
	// to on the last successful apply. Docker-down reconciles reuse them
	// (the ports pattern), so a daemon restart never flaps the chains.
	Bridges  []string `json:"bridges,omitempty"`
	Operator int      `json:"operator_exceptions,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
}

// commandRunner allows tests to drive the reconcile logic without iptables.
type commandRunner func(ctx context.Context, args ...string) ([]byte, error)

// Rules returns the exact FORWARD chain content, in order. Resolver addresses
// are validated /32 IPv4 hosts produced by ReadResolvers. The per-interface
// drops are scoped to the Docker bridge names the caller enumerated — never
// a `br+` wildcard, which also matches host-owned bridges.
func Rules(resolvers []string, bridges []string) [][]string {
	rules := [][]string{
		{"-m", "physdev", "--physdev-is-bridged", "-j", "RETURN"},
		// Metadata is refused on every forwarded path, not only the default
		// bridge names: custom-named bridges and macvlan-style tenants must
		// not reach 169.254.0.0/16 while the RFC1918 blocks stay scoped.
		{"-d", "169.254.0.0/16", "-j", "DROP"},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
		// A flow Docker DNATed (a container reaching a port the host
		// publishes, through the host's own address) gets what the internet
		// gets. Without this the RFC1918/ULA drops below cut the hairpin
		// (the WordPress loopback and WP-Cron behind the proxy).
		{"-m", "conntrack", "--ctstate", "DNAT", "-j", "RETURN"},
	}
	for _, r := range resolvers {
		rules = append(rules,
			[]string{"-p", "udp", "-d", r + "/32", "-m", "udp", "--dport", "53", "-j", "RETURN"},
			[]string{"-p", "tcp", "-d", r + "/32", "-m", "tcp", "--dport", "53", "-j", "RETURN"},
		)
	}
	blocked := [][]string{
		[]string{"-d", "10.0.0.0/8", "-j", "DROP"},
		[]string{"-d", "172.16.0.0/12", "-j", "DROP"},
		[]string{"-d", "192.168.0.0/16", "-j", "DROP"},
		[]string{"-p", "tcp", "-m", "conntrack", "--ctstate", "NEW", "-m", "multiport", "--dports", "25,465,587", "-j", "DROP"},
		[]string{"-m", "conntrack", "--ctstate", "NEW", "-m", "hashlimit", "--hashlimit-above", rateLimit, "--hashlimit-burst", rateBurst, "--hashlimit-mode", "srcip", "--hashlimit-name", limitTable, "-j", "DROP"},
	}
	for _, iface := range bridges {
		for _, rule := range blocked {
			rules = append(rules, append([]string{"-i", iface}, rule...))
		}
	}
	return append(rules, []string{"-j", "RETURN"})
}

// HostRules returns the host chain content with no exception (see host.go
// for the exceptions and the placement of the jump).
func HostRules(bridges []string) [][]string { return HostRulesFor("icmp", nil, bridges) }

func realRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "iptables", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.New("iptables rejected the egress baseline operation")
	}
	return out, nil
}

func fingerprint(rules [][]string) string {
	h := sha256.New()
	for _, rule := range rules {
		h.Write([]byte(strings.Join(rule, " ")))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reconcileChain installs or verifies one agent-owned chain linked at the top
// of parent. Idempotent: when the live chain content matches the fingerprint
// recorded by the previous apply of the same wanted rules and the link
// exists, nothing is executed. Only the agent-owned chain is flushed.
func reconcileChain(ctx context.Context, run commandRunner, chain, parent, label string, wanted [][]string, resolvers int, previous Status) (Status, error) {
	status := Status{Rules: len(wanted), Resolvers: resolvers, Wanted: fingerprint(wanted)}
	current, err := run(ctx, "-S", chain)
	if err != nil {
		if _, err = run(ctx, "-N", chain); err != nil {
			return status, errors.New(label + " chain cannot be created")
		}
		current = nil
	}
	parentRules, err := run(ctx, "-S", parent)
	if err != nil {
		return status, errors.New(label + " parent chain unavailable; docker may not be running yet")
	}
	linked := strings.Contains(string(parentRules), "-A "+parent+" -j "+chain+"\n") || strings.HasSuffix(strings.TrimRight(string(parentRules), "\n"), "-A "+parent+" -j "+chain)
	h := sha256Sum(current)
	if linked && len(current) != 0 && previous.Applied && previous.Wanted == status.Wanted && previous.Fingerprint == h {
		status.Applied = true
		status.Fingerprint = h
		return status, nil
	}
	if len(current) != 0 {
		if _, err = run(ctx, "-F", chain); err != nil {
			return status, errors.New(label + " chain cannot be reconciled")
		}
	}
	for _, rule := range wanted {
		if _, err = run(ctx, append([]string{"-A", chain}, rule...)...); err != nil {
			return status, errors.New(label + " baseline rule cannot be installed")
		}
	}
	current, err = run(ctx, "-S", chain)
	if err != nil {
		return status, errors.New(label + " chain cannot be verified after apply")
	}
	status.Fingerprint = sha256Sum(current)
	if !linked {
		if _, err = run(ctx, "-I", parent, "1", "-j", chain); err != nil {
			return status, errors.New(label + " chain cannot be linked into the docker forwarding path")
		}
	}
	status.Applied = true
	return status, nil
}

// apply reconciles the FORWARD chain only (the original v4 baseline shape,
// preserved for compatibility with the recorded fingerprints).
func apply(ctx context.Context, run commandRunner, stateDir string, resolvers []string, bridges []string) (Status, error) {
	status, err := reconcileChain(ctx, run, Chain, ParentChain, "egress", Rules(resolvers, bridges), len(resolvers), readStatus(stateDir))
	// Record the scope like apply6 and the host halves: Apply reads it back
	// as the Docker-down fallback.
	status.Bridges = bridges
	return status, err
}

// Apply installs or verifies the v4 baseline (FORWARD and host INPUT) and
// always records the outcome in <StateDir>/egress.json. A nil error means
// both halves are verified in place.
func Apply(ctx context.Context, stateDir string) error {
	defer enterApply(stateDir)()
	bridges := dockerBridgeInterfaces(ctx, realDocker, readStatus(stateDir).Bridges)
	errForward := applyAll(ctx, realRunner, "/etc/resolv.conf", stateDir, bridges)
	errHost := applyHostWith(ctx, hostFamily4(realRunner, realRestoreFor("iptables-restore")), stateDir, realDocker)
	return errors.Join(errForward, errHost)
}

func applyAll(ctx context.Context, run commandRunner, resolvPath, stateDir string, bridges []string) error {
	resolvers, err := ReadResolvers(resolvPath)
	var status Status
	if err == nil {
		status, err = apply(ctx, run, stateDir, resolvers, bridges)
	}
	if err != nil {
		status.Applied = false
		status.Error = err.Error()
	}
	status.LastAttempt = time.Now().UTC().Format(time.RFC3339)
	if writeErr := writeStatus(stateDir, status); writeErr != nil && err == nil {
		return errors.New("egress status could not be recorded")
	}
	return err
}

func statusPath(stateDir string) (string, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return "", errors.New("invalid egress state directory")
	}
	return filepath.Join(stateDir, "egress.json"), nil
}

func readStatus(stateDir string) Status {
	var f statusFile
	path, err := statusPath(stateDir)
	if err != nil {
		return Status{}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > statusLimit {
		return Status{}
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &f) != nil {
		return Status{}
	}
	return f.V4
}

func writeStatus(stateDir string, status Status) error {
	path, err := statusPath(stateDir)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("egress status path is not a regular file")
	}
	var f statusFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	f.V4 = status
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".egress-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
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
