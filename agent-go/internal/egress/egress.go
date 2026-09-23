// Package egress applies the host egress baseline for tenant containers.
//
// Security contract: an anonymous tenant
// container must not turn the host's public IPs into a spam, scan or C2
// source. The agent manages one iptables chain of its own, IMPREZA-EGRESS,
// linked at the top of DOCKER-USER (the chain Docker documents for operator
// rules and never flushes). Only the content of our own chain is ever
// reconciled; DOCKER-USER and every other chain are never flushed or
// reordered by us. The chain ends in RETURN: this is a baseline, never a
// default-deny.
//
// Scope and known limits of this phase:
//   - This file covers IPv4; egress6.go installs the separate IPv6 baseline.
//   - Rule precedence inside the chain: established flows, then DNS to the
//     host's own resolvers, then metadata (169.254.0.0/16), RFC1918
//     destinations, outbound SMTP (25/465/587) and a per-source-IP rate limit
//     on new flows. DNS is explicitly allowed to the resolvers configured in
//     /etc/resolv.conf so hosts whose resolver is RFC1918 (a LAN gateway) do
//     not break; a stub resolver (127.0.0.53) never traverses FORWARD and the
//     exception is simply inert there. DNS to hardcoded public resolvers is
//     deliberately not blocked in this phase; DNS-based exfiltration is part
//     of the edge/netflow backlog, not of this baseline.
//   - Only traffic entering FORWARD from Docker's default bridge names is
//     filtered. Same-bridge frames explicitly return, including when bridge
//     netfilter is enabled. Custom-named bridges, host INPUT and IPv6 are
//     outside this baseline. RETURN preserves downstream operator policy.
//   - Application failure is fail-open by design: the agent keeps running and
//     records the state in <StateDir>/egress.json. An egress firewall must
//     never take deploys down; the residual risk window is between Docker
//     start and agent start (the agent's systemd unit reapplies the baseline
//     on every boot, so no separate iptables persistence is installed).
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
	// Chain is the only chain whose content the agent ever reconciles.
	Chain = "IMPREZA-EGRESS"
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
}

// commandRunner allows tests to drive the reconcile logic without iptables.
type commandRunner func(ctx context.Context, args ...string) ([]byte, error)

// Rules returns the exact chain content, in order. Resolver addresses are
// validated /32 IPv4 hosts produced by ReadResolvers.
func Rules(resolvers []string) [][]string {
	rules := [][]string{
		{"-m", "physdev", "--physdev-is-bridged", "-j", "RETURN"},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
	}
	for _, r := range resolvers {
		rules = append(rules,
			[]string{"-p", "udp", "-d", r + "/32", "-m", "udp", "--dport", "53", "-j", "RETURN"},
			[]string{"-p", "tcp", "-d", r + "/32", "-m", "tcp", "--dport", "53", "-j", "RETURN"},
		)
	}
	blocked := [][]string{
		[]string{"-d", "169.254.0.0/16", "-j", "DROP"},
		[]string{"-d", "10.0.0.0/8", "-j", "DROP"},
		[]string{"-d", "172.16.0.0/12", "-j", "DROP"},
		[]string{"-d", "192.168.0.0/16", "-j", "DROP"},
		[]string{"-p", "tcp", "-m", "conntrack", "--ctstate", "NEW", "-m", "multiport", "--dports", "25,465,587", "-j", "DROP"},
		[]string{"-m", "conntrack", "--ctstate", "NEW", "-m", "hashlimit", "--hashlimit-above", rateLimit, "--hashlimit-burst", rateBurst, "--hashlimit-mode", "srcip", "--hashlimit-name", limitTable, "-j", "DROP"},
	}
	for _, iface := range []string{"docker0", "br+"} {
		for _, rule := range blocked {
			rules = append(rules, append([]string{"-i", iface}, rule...))
		}
	}
	return append(rules, []string{"-j", "RETURN"})
}

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

// apply reconciles only the agent-owned chain and its link. Idempotent: when
// the live chain content matches the fingerprint recorded by the previous
// apply of the same wanted rules and the link exists, nothing is executed.
func apply(ctx context.Context, run commandRunner, stateDir string, resolvers []string) (Status, error) {
	wanted := Rules(resolvers)
	status := Status{Rules: len(wanted), Resolvers: len(resolvers), Wanted: fingerprint(wanted)}
	previous := readStatus(stateDir)
	current, err := run(ctx, "-S", Chain)
	if err != nil {
		if _, err = run(ctx, "-N", Chain); err != nil {
			return status, errors.New("egress chain cannot be created")
		}
		current = nil
	}
	parent, err := run(ctx, "-S", ParentChain)
	if err != nil {
		return status, errors.New("docker user chain unavailable; docker may not be running yet")
	}
	linked := strings.Contains(string(parent), "-A "+ParentChain+" -j "+Chain+"\n") || strings.HasSuffix(strings.TrimRight(string(parent), "\n"), "-A "+ParentChain+" -j "+Chain)
	h := sha256.Sum256(current)
	if linked && len(current) != 0 && previous.Applied && previous.Wanted == status.Wanted && previous.Fingerprint == hex.EncodeToString(h[:]) {
		status.Applied = true
		status.Fingerprint = previous.Fingerprint
		return status, nil
	}
	if len(current) != 0 {
		if _, err = run(ctx, "-F", Chain); err != nil {
			return status, errors.New("egress chain cannot be reconciled")
		}
	}
	for _, rule := range wanted {
		if _, err = run(ctx, append([]string{"-A", Chain}, rule...)...); err != nil {
			return status, errors.New("egress baseline rule cannot be installed")
		}
	}
	current, err = run(ctx, "-S", Chain)
	if err != nil {
		return status, errors.New("egress chain cannot be verified after apply")
	}
	h = sha256.Sum256(current)
	status.Fingerprint = hex.EncodeToString(h[:])
	if !linked {
		if _, err = run(ctx, "-I", ParentChain, "1", "-j", Chain); err != nil {
			return status, errors.New("egress chain cannot be linked into the docker forwarding path")
		}
	}
	status.Applied = true
	return status, nil
}

// Apply installs or verifies the baseline and always records the outcome in
// <StateDir>/egress.json. A nil error means the baseline is verified in place.
func Apply(ctx context.Context, stateDir string) error {
	return applyAll(ctx, realRunner, "/etc/resolv.conf", stateDir)
}

func applyAll(ctx context.Context, run commandRunner, resolvPath, stateDir string) error {
	resolvers, err := ReadResolvers(resolvPath)
	var status Status
	if err == nil {
		status, err = apply(ctx, run, stateDir, resolvers)
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
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return Status{}
	}
	raw, err := readStatusFile(path)
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
	if raw, err := readStatusFile(path); err == nil {
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
