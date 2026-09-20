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
		if strings.HasSuffix(line, "-j DROP") && !(strings.HasPrefix(line, "-i docker0 ") || strings.HasPrefix(line, "-i br+ ")) {
			t.Fatal("drop affects ingress or unrelated forwarding")
		}
	}
	if strings.Join(rules[0], " ") != "-m physdev --physdev-is-bridged -j RETURN" {
		t.Fatal("same bridge exemption absent")
	}
	if strings.Join(rules[len(rules)-1], " ") != "-j RETURN" {
		t.Fatal("operator continuation absent")
	}
	for _, iface := range []string{"docker0", "br+"} {
		for _, dest := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
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
	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
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
