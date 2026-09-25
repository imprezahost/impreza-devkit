package egress

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRulesScopeAndOperatorPolicy(t *testing.T) {
	rules := Rules([]string{"10.0.0.2", "1.1.1.1"})
	for _, rule := range rules {
		line := strings.Join(rule, " ")
		if strings.Contains(line, "ACCEPT") {
			t.Fatal("baseline bypasses later operator policy")
		}
		if strings.HasSuffix(line, "-j DROP") && line != "-d 169.254.0.0/16 -j DROP" && !(strings.HasPrefix(line, "-i docker0 ") || strings.HasPrefix(line, "-i br+ ")) {
			t.Fatal("drop affects ingress or unrelated forwarding")
		}
	}
	if strings.Join(rules[0], " ") != "-m physdev --physdev-is-bridged -j RETURN" {
		t.Fatal("same bridge exemption absent")
	}
	// The metadata range is the one interface-agnostic drop: custom-named
	// bridges and macvlan-style tenant paths must not reach it either.
	if strings.Join(rules[1], " ") != "-d 169.254.0.0/16 -j DROP" {
		t.Fatal("interface-agnostic metadata drop absent or misplaced")
	}
	if strings.Join(rules[len(rules)-1], " ") != "-j RETURN" {
		t.Fatal("operator continuation absent")
	}
	for _, iface := range []string{"docker0", "br+"} {
		for _, dest := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
			found := false
			for _, rule := range rules {
				if strings.Join(rule, " ") == "-i "+iface+" -d "+dest+" -j DROP" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing restriction %s %s", iface, dest)
			}
		}
	}
}

func TestHostRulesScopeAndShape(t *testing.T) {
	rules := HostRules()
	if len(rules) != 6 {
		t.Fatalf("unexpected host rule count: %d", len(rules))
	}
	for _, iface := range []string{"docker0", "br+"} {
		est := false
		icmp := false
		drop := false
		for _, rule := range rules {
			line := strings.Join(rule, " ")
			if !strings.HasPrefix(line, "-i "+iface+" ") {
				continue
			}
			switch {
			case strings.Contains(line, "ESTABLISHED,RELATED") && strings.HasSuffix(line, "-j RETURN"):
				est = true
			case strings.Contains(line, "-p icmp") && strings.HasSuffix(line, "-j RETURN"):
				icmp = true
			case strings.HasSuffix(line, "-j DROP"):
				drop = true
			}
		}
		if !est || !icmp || !drop {
			t.Fatalf("host rules incomplete for %s: established=%v icmp=%v drop=%v", iface, est, icmp, drop)
		}
	}
	// Nothing matches traffic that does not arrive from a Docker bridge: the
	// operator's INPUT policy (and remote management) stays in charge.
	for _, rule := range rules {
		line := strings.Join(rule, " ")
		if !strings.HasPrefix(line, "-i docker0 ") && !strings.HasPrefix(line, "-i br+ ") {
			t.Fatalf("host rule without bridge scope: %s", line)
		}
		if strings.Contains(line, "ACCEPT") {
			t.Fatal("host chain accepts instead of returning to operator policy")
		}
	}
}

func TestReadResolvers(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	got, err := ReadResolvers(write("plain", "# comment\nnameserver 10.0.0.2\n\noptions ndots:2\nnameserver 1.1.1.1 ; trailing\nnameserver 10.0.0.2\nnameserver fd00::1\nnameserver not-an-ip\nnameserver\n"))
	if err != nil || !reflect.DeepEqual(got, []string{"10.0.0.2", "1.1.1.1"}) {
		t.Fatalf("resolver parse mismatch: %v %v", got, err)
	}
	if _, err = ReadResolvers(write("empty", "search example.test\n")); err == nil {
		t.Fatal("missing resolvers accepted")
	}
	if _, err = ReadResolvers(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("absent resolver configuration accepted")
	}
	big := write("big", "nameserver 1.1.1.1\n"+strings.Repeat("# padding\n", resolvConfLimit/10))
	if _, err = ReadResolvers(big); err == nil {
		t.Fatal("oversized resolver configuration accepted")
	}
	// Symlinks resolve only to regular files: the systemd-resolved stub layout
	// is standard and must work; dangling or device targets are refused.
	target := write("target", "nameserver 192.168.1.1\n")
	link := filepath.Join(dir, "link")
	if err = os.Symlink(target, link); err != nil {
		t.Logf("symlink creation unavailable on this platform: %v", err)
	} else {
		got, err = ReadResolvers(link)
		if err != nil || !reflect.DeepEqual(got, []string{"192.168.1.1"}) {
			t.Fatalf("standard resolver symlink refused: %v %v", got, err)
		}
		dangling := filepath.Join(dir, "dangling")
		if err = os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
			t.Fatal(err)
		}
		if _, err = ReadResolvers(dangling); err == nil {
			t.Fatal("dangling resolver symlink followed")
		}
	}
	if _, err = ReadResolvers(os.DevNull); err == nil {
		t.Fatal("device accepted as resolver configuration")
	}
}

// fakeIPTables simulates the exact command surface the baseline reconciles.
type fakeIPTables struct {
	chains    map[string][][]string
	mutations int
	failAll   bool
}

func (f *fakeIPTables) run(_ context.Context, args ...string) ([]byte, error) {
	if f.failAll {
		return nil, errors.New("iptables absent")
	}
	name := func() string {
		for i, a := range args {
			if (a == "-S" || a == "-N" || a == "-F" || a == "-A") && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}()
	switch args[0] {
	case "-S":
		rules, ok := f.chains[name]
		if !ok {
			return nil, errors.New("no such chain")
		}
		out := "-N " + name + "\n"
		for _, r := range rules {
			out += "-A " + name + " " + strings.Join(r, " ") + "\n"
		}
		return []byte(out), nil
	case "-N":
		f.mutations++
		f.chains[name] = nil
	case "-F":
		f.mutations++
		if _, ok := f.chains[name]; !ok {
			return nil, errors.New("no such chain")
		}
		f.chains[name] = nil
	case "-A":
		f.mutations++
		f.chains[name] = append(f.chains[name], args[2:])
	case "-C":
		for _, r := range f.chains[args[1]] {
			if reflect.DeepEqual(r, args[2:]) {
				return nil, nil
			}
		}
		return nil, errors.New("rule absent")
	case "-I":
		f.mutations++
		f.chains[args[1]] = append([][]string{args[3:]}, f.chains[args[1]]...)
	default:
		return nil, errors.New("unexpected iptables operation")
	}
	return nil, nil
}

func applyFixture(t *testing.T) (context.Context, *fakeIPTables, string, string) {
	t.Helper()
	dir := t.TempDir()
	resolv := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 10.0.0.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeIPTables{chains: map[string][][]string{ParentChain: {}}}
	return context.Background(), fake, resolv, dir
}

func readApplied(t *testing.T, dir string) Status {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "egress.json"))
	if err != nil {
		t.Fatal("egress status not recorded")
	}
	var f statusFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.V4
}

func TestApplyIdempotentAndReconciles(t *testing.T) {
	ctx, fake, resolv, dir := applyFixture(t)
	if err := applyAll(ctx, fake.run, resolv, dir); err != nil {
		t.Fatal(err)
	}
	status := readApplied(t, dir)
	if !status.Applied || status.Error != "" || status.LastAttempt == "" || status.Fingerprint == "" || status.Rules != len(Rules([]string{"10.0.0.2"})) || status.Resolvers != 1 {
		t.Fatalf("initial apply status mismatch: %+v", status)
	}
	if len(fake.chains[Chain]) != status.Rules || len(fake.chains[ParentChain]) != 1 || !reflect.DeepEqual(fake.chains[ParentChain][0], []string{"-j", Chain}) {
		t.Fatal("chain content or parent link incorrect after apply")
	}
	mutations := fake.mutations
	if err := applyAll(ctx, fake.run, resolv, dir); err != nil {
		t.Fatal(err)
	}
	if fake.mutations != mutations {
		t.Fatal("second apply mutated the firewall")
	}
	// External drift to our chain is reconciled; the parent chain is untouched.
	fake.chains[Chain] = nil
	if err := applyAll(ctx, fake.run, resolv, dir); err != nil {
		t.Fatal(err)
	}
	if len(fake.chains[Chain]) != status.Rules || len(fake.chains[ParentChain]) != 1 {
		t.Fatal("drifted chain was not reconciled")
	}
	// A resolver change replaces the chain content.
	if err := os.WriteFile(resolv, []byte("nameserver 192.168.1.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := applyAll(ctx, fake.run, resolv, dir); err != nil {
		t.Fatal(err)
	}
	if readApplied(t, dir).Fingerprint == status.Fingerprint {
		t.Fatal("resolver change did not reapply the baseline")
	}
	// A vanished parent chain is an explicit fail-open error, never a flush.
	delete(fake.chains, ParentChain)
	if err := applyAll(ctx, fake.run, resolv, dir); err == nil || readApplied(t, dir).Applied {
		t.Fatal("missing docker chain was not reported fail-open")
	}
}

func TestApplyFailOpenWithoutIPTables(t *testing.T) {
	ctx, fake, resolv, dir := applyFixture(t)
	fake.failAll = true
	err := applyAll(ctx, fake.run, resolv, dir)
	if err == nil {
		t.Fatal("iptables absence was not reported")
	}
	status := readApplied(t, dir)
	if status.Applied || status.Error == "" || status.LastAttempt == "" {
		t.Fatalf("fail-open status not recorded: %+v", status)
	}
	if len(fake.chains[Chain]) != 0 {
		t.Fatal("failed apply left partial rules")
	}
}

// The host INPUT half reconciles like the FORWARD half (create, drift
// refill, idempotent no-op) but links into INPUT, records its own status
// section, and never flushes anything but the agent-owned chain.
func TestApplyHostChain(t *testing.T) {
	ctx, fake, _, dir := applyFixture(t)
	fake.chains["INPUT"] = [][]string{{"-A", "INPUT", "-j", "ACCEPT"}}
	if err := applyHostAll(ctx, fake.run, dir); err != nil {
		t.Fatal(err)
	}
	wanted := HostRules()
	if len(fake.chains[HostChain]) != len(wanted) {
		t.Fatalf("host chain content mismatch: %d rules, want %d", len(fake.chains[HostChain]), len(wanted))
	}
	if len(fake.chains["INPUT"]) != 2 || !reflect.DeepEqual(fake.chains["INPUT"][0], []string{"-j", HostChain}) || !reflect.DeepEqual(fake.chains["INPUT"][1], []string{"-A", "INPUT", "-j", "ACCEPT"}) {
		t.Fatal("host chain not linked at the top of INPUT without touching operator rules")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "egress.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f statusFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.V4Host == nil || !f.V4Host.Applied || f.V4Host.Fingerprint == "" || f.V4Host.Rules != len(wanted) || f.V4Host.Resolvers != 0 {
		t.Fatalf("host status section not recorded: %+v", f.V4Host)
	}
	mutations := fake.mutations
	if err := applyHostAll(ctx, fake.run, dir); err != nil {
		t.Fatal(err)
	}
	if fake.mutations != mutations {
		t.Fatal("second host apply mutated the firewall")
	}
	// External drift to the host chain reconciles; INPUT keeps its shape.
	fake.chains[HostChain] = nil
	if err := applyHostAll(ctx, fake.run, dir); err != nil {
		t.Fatal(err)
	}
	if len(fake.chains[HostChain]) != len(wanted) || len(fake.chains["INPUT"]) != 2 {
		t.Fatal("drifted host chain was not reconciled or INPUT was rewritten")
	}
	// A vanished INPUT chain is a fail-open error, never a flush.
	delete(fake.chains, "INPUT")
	if err := applyHostAll(ctx, fake.run, dir); err == nil {
		t.Fatal("missing INPUT was not reported fail-open")
	}
}
