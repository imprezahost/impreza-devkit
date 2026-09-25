package egress

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The v6 rules must carry the same contract as v4 in the v6 address space:
// same-bridge RETURN, established RETURN, DNS exceptions before the drops,
// link-local and ULA drops, the v4-mapped metadata space, SMTP and the
// per-source rate limit, and a final RETURN (baseline, not default-deny).
// Custom bridges are covered destination-side: there is no interface-name
// filter in the v6 chain — the physdev RETURN at the top is the only bridge
// rule, so traffic from ANY bridge hits the destination blocks.
func TestRules6Shape(t *testing.T) {
	rules := Rules6([]string{"2001:db8::53"})
	var sb strings.Builder
	for _, r := range rules {
		sb.WriteString(strings.Join(r, " "))
		sb.WriteString("\n")
	}
	joined := sb.String()
	for _, want := range []string{
		"--physdev-is-bridged -j RETURN",
		"--ctstate ESTABLISHED,RELATED -j RETURN",
		"-d 2001:db8::53/128 -m udp --dport 53 -j RETURN",
		"-d fe80::/10 -j DROP",
		"-d fc00::/7 -j DROP",
		"-d ::ffff:169.254.0.0/112 -j DROP",
		"--dports 25,465,587 -j DROP",
		"--hashlimit-above 300/minute",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("v6 rules missing %q:\n%s", want, joined)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(joined), "-j RETURN") {
		t.Fatal("v6 chain must end in RETURN (baseline, never default-deny)")
	}
	// No interface-name filter: custom bridges are covered by destination.
	for _, banned := range []string{"-i docker0", "-i br+"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("v6 chain must not filter by interface name (custom bridges would bypass): %s", banned)
		}
	}
}

func TestReadResolvers6FiltersFamily(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 192.0.2.1\nnameserver 2001:db8::53\nnameserver 2001:db8::53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResolvers6(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "2001:db8::53" {
		t.Fatalf("v6 resolvers drifted: %v", got)
	}
	// A v4-only resolv.conf is not an error: the v6 baseline still runs.
	if err := os.WriteFile(path, []byte("nameserver 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = ReadResolvers6(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("v4-only host must yield zero v6 resolvers, got %v", got)
	}
}

// The v6 apply shares the reconcile contract: create-if-missing, flush and
// refill on drift, link into DOCKER-USER, idempotent no-op when the
// fingerprint matches, and the outcome recorded under the v6 section of
// egress.json alongside the v4 status.
func TestApply6Reconcile(t *testing.T) {
	var calls [][]string
	chainDump := "-A " + Chain + " -j RETURN\n"
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, args)
		switch {
		case strings.HasPrefix(call, "-S "+Chain):
			return []byte(chainDump), nil
		case strings.HasPrefix(call, "-S "+ParentChain):
			return []byte("-A " + ParentChain + " -j " + Chain + "\n"), nil
		case call == "-F "+Chain, strings.HasPrefix(call, "-A "+Chain), call == "-N "+Chain:
			return nil, nil
		case strings.HasPrefix(call, "-I "+ParentChain):
			return nil, nil
		}
		return nil, errors.New("unexpected ip6tables call: " + call)
	}
	dir := t.TempDir()
	status, err := apply6(context.Background(), run, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Applied {
		t.Fatal("v6 apply did not report applied")
	}
	if err := writeStatus6(dir, status); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "egress.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"v6"`) {
		t.Fatalf("v6 status not recorded: %s", raw)
	}
	// Second apply with matching fingerprint makes no mutating calls.
	before := len(calls)
	if _, err := apply6(context.Background(), run, dir, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls[before:] {
		first := c[0]
		if first == "-F" || first == "-A" || first == "-I" {
			t.Fatalf("idempotent v6 re-apply mutated the firewall: %v", c)
		}
	}
}

// The v6 host INPUT half keeps ICMPv6 (NDP/RS) and established flows from
// Docker bridges returning to the operator's INPUT policy and drops the
// rest; every rule is bridge-scoped so remote management never matches.
func TestHostRules6Shape(t *testing.T) {
	rules := HostRules6()
	if len(rules) != 6 {
		t.Fatalf("unexpected v6 host rule count: %d", len(rules))
	}
	var sb strings.Builder
	for _, r := range rules {
		sb.WriteString(strings.Join(r, " "))
		sb.WriteString("\n")
	}
	joined := sb.String()
	for _, want := range []string{
		"-i docker0 -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN",
		"-i docker0 -p ipv6-icmp -j RETURN",
		"-i docker0 -j DROP",
		"-i br+ -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN",
		"-i br+ -p ipv6-icmp -j RETURN",
		"-i br+ -j DROP",
	} {
		if !strings.Contains(joined, want+"\n") {
			t.Fatalf("v6 host rules missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ACCEPT") {
		t.Fatal("v6 host chain accepts instead of returning to operator policy")
	}
}

func TestApplyHost6Reconcile(t *testing.T) {
	var calls [][]string
	hostDump := "-A " + HostChain + " -i docker0 -j DROP\n"
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, args)
		switch {
		case strings.HasPrefix(call, "-S "+HostChain):
			return []byte(hostDump), nil
		case strings.HasPrefix(call, "-S INPUT"):
			return []byte("-A INPUT -j " + HostChain + "\n"), nil
		case call == "-F "+HostChain, strings.HasPrefix(call, "-A "+HostChain), call == "-N "+HostChain:
			return nil, nil
		case strings.HasPrefix(call, "-I INPUT"):
			return nil, nil
		}
		return nil, errors.New("unexpected ip6tables call: " + call)
	}
	dir := t.TempDir()
	if err := applyHostAll6(context.Background(), run, dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "egress.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"v6_host"`) {
		t.Fatalf("v6 host status not recorded: %s", raw)
	}
}
