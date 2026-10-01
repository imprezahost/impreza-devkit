package egress

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// hostFake is one family: INPUT, the host chain, and iptables-restore
// --noflush transactions that apply whole or not at all.
type hostFake struct {
	chains      map[string][][]string
	order       []string
	restores    int
	failRestore bool
	commands    []string
}

func newHostFake(input ...string) *hostFake {
	f := &hostFake{chains: map[string][][]string{"INPUT": nil}, order: []string{"INPUT"}}
	for _, r := range input {
		f.chains["INPUT"] = append(f.chains["INPUT"], strings.Fields(r))
	}
	return f
}

func (f *hostFake) tables(_ context.Context, args ...string) ([]byte, error) {
	f.commands = append(f.commands, strings.Join(args, " "))
	if len(args) == 2 && args[0] == "-S" {
		rules, ok := f.chains[args[1]]
		if !ok {
			return nil, errors.New("no such chain")
		}
		out := "-N " + args[1] + "\n"
		if args[1] == "INPUT" {
			out = "-P INPUT ACCEPT\n"
		}
		for _, r := range rules {
			out += "-A " + args[1] + " " + strings.Join(r, " ") + "\n"
		}
		return []byte(out), nil
	}
	return nil, errors.New("host half must only read with iptables, and write with iptables-restore: " + strings.Join(args, " "))
}

func (f *hostFake) restore(_ context.Context, input string) error {
	if f.failRestore {
		return errors.New("injected")
	}
	next := map[string][][]string{}
	for k, v := range f.chains {
		next[k] = append([][]string{}, v...)
	}
	lines := strings.Split(strings.TrimSpace(input), "\n")
	if lines[0] != "*filter" || lines[len(lines)-1] != "COMMIT" {
		return errors.New("not one filter transaction")
	}
	for _, l := range lines[1 : len(lines)-1] {
		fields := strings.Fields(l)
		switch {
		case strings.HasPrefix(l, ":"):
			next[strings.TrimPrefix(fields[0], ":")] = nil
		case fields[0] == "-A":
			if _, ok := next[fields[1]]; !ok {
				return errors.New("append to a missing chain")
			}
			next[fields[1]] = append(next[fields[1]], fields[2:])
		case fields[0] == "-I":
			if _, ok := next[fields[1]]; !ok {
				return errors.New("insert into a missing chain")
			}
			at := 1
			rest := fields[2:]
			if len(rest) > 0 && rest[0] != "-j" {
				at, _ = strconv.Atoi(rest[0])
				rest = rest[1:]
			}
			rules := next[fields[1]]
			if at < 1 || at > len(rules)+1 {
				return errors.New("insert position out of range")
			}
			next[fields[1]] = append(append(append([][]string{}, rules[:at-1]...), rest), rules[at-1:]...)
		case fields[0] == "-D":
			rules, want, found := next[fields[1]], strings.Join(fields[2:], " "), false
			for i, r := range rules {
				if strings.Join(r, " ") == want {
					next[fields[1]] = append(rules[:i:i], rules[i+1:]...)
					found = true
					break
				}
			}
			if !found {
				return errors.New("delete of an absent rule")
			}
		case fields[0] == "-X":
			for _, r := range next["INPUT"] {
				if strings.Join(r, " ") == "-j "+fields[1] {
					return errors.New("delete of a referenced chain")
				}
			}
			delete(next, fields[1])
		default:
			return errors.New("unexpected restore line: " + l)
		}
	}
	f.chains = next
	f.restores++
	return nil
}

func (f *hostFake) input() []string {
	var out []string
	for _, r := range f.chains["INPUT"] {
		out = append(out, strings.Join(r, " "))
	}
	return out
}

// dockerFake answers docker ps / inspect with fixed port maps.
type dockerFake struct {
	down  bool
	ports []string // JSON of .NetworkSettings.Ports, one per container
}

func (d *dockerFake) run(_ context.Context, args ...string) ([]byte, error) {
	if d.down {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}
	if args[0] == "ps" {
		var ids []string
		for i := range d.ports {
			ids = append(ids, fmt.Sprintf("c%063d", i))
		}
		return []byte(strings.Join(ids, "\n") + "\n"), nil
	}
	if args[0] == "inspect" {
		return []byte(strings.Join(d.ports, "\n") + "\n"), nil
	}
	return nil, errors.New("unexpected docker call")
}

const proxyPorts = `{"443/tcp":[{"HostIp":"0.0.0.0","HostPort":"443"},{"HostIp":"::","HostPort":"443"}],"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"80"}]}`
const loopbackOnly = `{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"5432"}]}`
const hostPortsApp = `{"27015/udp":[{"HostIp":"0.0.0.0","HostPort":"27015"}],"25565/tcp":[{"HostIp":"","HostPort":"25565"}]}`

func applyHost4(t *testing.T, f *hostFake, d *dockerFake, dir string) error {
	t.Helper()
	return applyHostWith(context.Background(), hostFamily4(f.tables, f.restore), dir, d.run)
}

func has(rules [][]string, line string) bool {
	for _, r := range rules {
		if strings.Join(r, " ") == line {
			return true
		}
	}
	return false
}

// The operator's rules come first: the jump is appended, never inserted.
func TestHostJumpIsLastSoOperatorDecisionsWin(t *testing.T) {
	f := newHostFake("-j ufw-before-input", "-s 172.16.0.0/12 -p tcp -m tcp --dport 8080 -j ACCEPT")
	d := &dockerFake{}
	if err := applyHost4(t, f, d, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	in := f.input()
	if len(in) != 3 || in[0] != "-j ufw-before-input" || in[1] != "-s 172.16.0.0/12 -p tcp -m tcp --dport 8080 -j ACCEPT" || in[2] != "-j "+HostChain {
		t.Fatalf("INPUT: %q", in)
	}
	if f.restores != 1 {
		t.Fatalf("want one transaction, got %d", f.restores)
	}
}

// Upgrading from 0.6.21-0.6.24: the jump at the top moves to the end in the
// same transaction (no instant without it, no instant with two).
func TestHostUpgradeMovesTheTopJumpToTheEnd(t *testing.T) {
	f := newHostFake("-j "+HostChain, "-j ufw-before-input", "-s 172.16.0.0/12 -j ACCEPT")
	f.chains[HostChain] = HostRules()
	d := &dockerFake{}
	if err := applyHost4(t, f, d, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	in := f.input()
	if len(in) != 3 || in[0] != "-j ufw-before-input" || in[2] != "-j "+HostChain || f.restores != 1 {
		t.Fatalf("INPUT after upgrade: %q (%d restores)", in, f.restores)
	}
}

// An operator (or ufw enable) appending to INPUT after the agent pushes the
// jump off the end; the reconcile moves it back, and does nothing while it
// is already last.
func TestHostReconcileKeepsTheJumpLastAndIsIdempotent(t *testing.T) {
	f := newHostFake("-j ufw-before-input")
	d := &dockerFake{ports: []string{proxyPorts}}
	dir := t.TempDir()
	if err := applyHost4(t, f, d, dir); err != nil {
		t.Fatal(err)
	}
	if err := applyHost4(t, f, d, dir); err != nil || f.restores != 1 {
		t.Fatalf("second apply rewrote a stable chain (%d restores): %v", f.restores, err)
	}
	f.chains["INPUT"] = append(f.chains["INPUT"], []string{"-j", "ufw-after-input"})
	if err := applyHost4(t, f, d, dir); err != nil {
		t.Fatal(err)
	}
	in := f.input()
	if in[len(in)-1] != "-j "+HostChain || strings.Count(strings.Join(in, "\n"), HostChain) != 1 || f.restores != 2 {
		t.Fatalf("jump not moved back to the end: %q", in)
	}
	// A flushed host chain is refilled.
	f.chains[HostChain] = nil
	if err := applyHost4(t, f, d, dir); err != nil || len(f.chains[HostChain]) == 0 {
		t.Fatalf("drift not reconciled: %v", err)
	}
}

// The hairpin: the ports Docker publishes on a public address return; a
// loopback-only publish does not (a bridge cannot reach it anyway); a
// stopped Docker keeps the last list.
func TestHostPublishedPortsReturnAndSurviveDockerDown(t *testing.T) {
	f := newHostFake()
	d := &dockerFake{ports: []string{proxyPorts, loopbackOnly, hostPortsApp}}
	dir := t.TempDir()
	if err := applyHost4(t, f, d, dir); err != nil {
		t.Fatal(err)
	}
	for _, iface := range []string{"docker0", "br+"} {
		for _, pp := range []string{"tcp -m tcp --dport 80", "tcp -m tcp --dport 443", "udp -m udp --dport 27015", "tcp -m tcp --dport 25565"} {
			if !has(f.chains[HostChain], "-i "+iface+" -p "+pp+" -j RETURN") {
				t.Fatalf("%s: published %s not returned", iface, pp)
			}
		}
		if has(f.chains[HostChain], "-i "+iface+" -p tcp -m tcp --dport 5432 -j RETURN") {
			t.Fatal("loopback-only publish exempted")
		}
		last := f.chains[HostChain]
		_ = last
	}
	// DROP stays the last rule per interface.
	rules := f.chains[HostChain]
	if strings.Join(rules[len(rules)-1], " ") != "-i br+ -j DROP" {
		t.Fatalf("host chain does not end in the bridge drop: %q", rules[len(rules)-1])
	}
	before := f.restores
	d.down = true
	if err := applyHost4(t, f, d, dir); err != nil || f.restores != before || !has(f.chains[HostChain], "-i br+ -p tcp -m tcp --dport 443 -j RETURN") {
		t.Fatalf("docker down changed the exceptions: %v", err)
	}
	// A new deployment publishes a port: the next reconcile returns it.
	d.down = false
	d.ports = append(d.ports, `{"8443/tcp":[{"HostIp":"0.0.0.0","HostPort":"8443"}]}`)
	if err := applyHost4(t, f, d, dir); err != nil || !has(f.chains[HostChain], "-i docker0 -p tcp -m tcp --dport 8443 -j RETURN") {
		t.Fatalf("new published port not returned: %v", err)
	}
}

// The operator's persistent exceptions and opt-out.
func TestHostOperatorPolicyAllowDisableEnable(t *testing.T) {
	f := newHostFake("-j ufw-before-input")
	d := &dockerFake{}
	dir := t.TempDir()
	if err := SaveHostPolicy(dir, HostPolicy{Allow: []string{"TCP/8080", "udp/53", "tcp/8080"}}); err != nil {
		t.Fatal(err)
	}
	p, err := LoadHostPolicy(dir)
	if err != nil || strings.Join(p.Allow, ",") != "udp/53,tcp/8080" {
		t.Fatalf("policy %+v %v", p, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, hostPolicyFile)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("policy mode %v", info.Mode().Perm())
	}
	if err := applyHost4(t, f, d, dir); err != nil || !has(f.chains[HostChain], "-i br+ -p tcp -m tcp --dport 8080 -j RETURN") || !has(f.chains[HostChain], "-i docker0 -p udp -m udp --dport 53 -j RETURN") {
		t.Fatalf("operator exceptions not rendered: %v", err)
	}
	for _, bad := range []string{"tcp/0", "tcp/70000", "icmp/1", "tcp/80;reboot", "tcp/ 80", ""} {
		if err := SaveHostPolicy(dir, HostPolicy{Allow: []string{bad}}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if err := SaveHostPolicy(dir, HostPolicy{Disabled: true, Allow: []string{"tcp/8080"}}); err != nil {
		t.Fatal(err)
	}
	if err := applyHost4(t, f, d, dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.chains[HostChain]; ok || strings.Contains(strings.Join(f.input(), "\n"), HostChain) {
		t.Fatalf("disabled half left rules: %q", f.input())
	}
	if st := readHostStatus4(dir); !st.Disabled || st.Applied {
		t.Fatalf("disabled status %+v", st)
	}
	// Reboot and reconcile keep it off.
	if err := applyHost4(t, f, d, dir); err != nil || strings.Contains(strings.Join(f.input(), "\n"), HostChain) {
		t.Fatalf("reconcile turned it back on: %v", err)
	}
	if err := SaveHostPolicy(dir, HostPolicy{Allow: []string{"tcp/8080"}}); err != nil {
		t.Fatal(err)
	}
	if err := applyHost4(t, f, d, dir); err != nil || f.input()[len(f.input())-1] != "-j "+HostChain {
		t.Fatalf("enable did not relink last: %v %q", err, f.input())
	}
}

// A policy file that cannot be read never makes the agent guess: nothing
// changes and the error is recorded.
func TestHostInvalidPolicyChangesNothing(t *testing.T) {
	f := newHostFake()
	d := &dockerFake{}
	dir := t.TempDir()
	if err := applyHost4(t, f, d, dir); err != nil {
		t.Fatal(err)
	}
	before := f.restores
	if err := os.WriteFile(filepath.Join(dir, hostPolicyFile), []byte(`{"version":1,"allow":["tcp/80;x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyHost4(t, f, d, dir); err == nil || f.restores != before {
		t.Fatalf("invalid policy applied (%d restores): %v", f.restores, err)
	}
	if st := readHostStatus4(dir); st.Error == "" {
		t.Fatal("invalid policy not recorded")
	}
}

// A failed transaction is fail-open and leaves the previous rules alone.
func TestHostRestoreFailureChangesNothing(t *testing.T) {
	f := newHostFake("-j " + HostChain)
	f.chains[HostChain] = HostRules()
	f.failRestore = true
	dir := t.TempDir()
	if err := applyHost4(t, f, &dockerFake{}, dir); err == nil {
		t.Fatal("failure not reported")
	}
	if in := f.input(); len(in) != 1 || in[0] != "-j "+HostChain || len(f.chains[HostChain]) != len(HostRules()) {
		t.Fatalf("a failed transaction changed the firewall: %q", in)
	}
	if st := readHostStatus4(dir); st.Applied || st.Error == "" {
		t.Fatalf("status %+v", st)
	}
}

// One journal line on change and once per process; counts only.
func TestHostNotifyCountsNoAddresses(t *testing.T) {
	announced.Delete("egress host")
	var lines []string
	Notify = func(msg string, args ...any) { lines = append(lines, fmt.Sprint(append([]any{msg}, args...)...)) }
	defer func() { Notify = nil }()
	f := newHostFake()
	d := &dockerFake{ports: []string{proxyPorts}}
	dir := t.TempDir()
	if err := SaveHostPolicy(dir, HostPolicy{Allow: []string{"tcp/8080"}}); err != nil {
		t.Fatal(err)
	}
	_ = applyHost4(t, f, d, dir)
	_ = applyHost4(t, f, d, dir) // unchanged, already announced: silent
	if len(lines) != 1 || !strings.Contains(lines[0], "egress host baseline applied") || !strings.Contains(lines[0], "last INPUT rule") {
		t.Fatalf("journal lines: %q", lines)
	}
	if strings.Contains(lines[0], "172.") || strings.Contains(lines[0], "0.0.0.0") {
		t.Fatalf("journal line carries an address: %q", lines[0])
	}
	d.ports = nil
	_ = applyHost4(t, f, d, dir)
	if len(lines) != 2 {
		t.Fatalf("a change was not reported: %q", lines)
	}
}

// Same behaviour for IPv6, with ICMPv6 (NDP) returned.
func TestHost6JumpLastWithICMPv6AndExceptions(t *testing.T) {
	f := newHostFake("-j "+HostChain, "-j ufw6-before-input")
	f.chains[HostChain] = HostRules6()
	d := &dockerFake{ports: []string{proxyPorts}}
	dir := t.TempDir()
	if err := applyHostWith(context.Background(), hostFamily6(f.tables, f.restore), dir, d.run); err != nil {
		t.Fatal(err)
	}
	in := f.input()
	if in[len(in)-1] != "-j "+HostChain || in[0] != "-j ufw6-before-input" {
		t.Fatalf("v6 INPUT %q", in)
	}
	if !has(f.chains[HostChain], "-i br+ -p ipv6-icmp -j RETURN") || !has(f.chains[HostChain], "-i br+ -p tcp -m tcp --dport 443 -j RETURN") {
		t.Fatal("v6 host chain misses ICMPv6 or the published port")
	}
	if st := readHostStatus6(dir); !st.Applied || st.Position != "last" || strings.Join(st.Ports, ",") != "tcp/80,tcp/443" {
		t.Fatalf("v6 status %+v", st)
	}
}

// The FORWARD hairpin: a DNATed flow (a published port reached through the
// host address) returns after the metadata drop and before every private
// destination drop, in both families.
func TestForwardReturnsDNATBeforePrivateDrops(t *testing.T) {
	for name, rules := range map[string][][]string{"v4": Rules([]string{"10.0.0.2"}), "v6": Rules6(nil)} {
		dnat, meta, firstDrop := -1, -1, -1
		for i, r := range rules {
			line := strings.Join(r, " ")
			switch {
			case line == "-m conntrack --ctstate DNAT -j RETURN":
				dnat = i
			case line == "-d 169.254.0.0/16 -j DROP":
				meta = i
			case strings.HasSuffix(line, "-j DROP") && firstDrop < 0 && line != "-d 169.254.0.0/16 -j DROP":
				firstDrop = i
			}
		}
		if dnat < 0 || firstDrop < 0 || dnat > firstDrop || (name == "v4" && (meta < 0 || meta > dnat)) {
			t.Fatalf("%s: dnat=%d metadata=%d first private drop=%d", name, dnat, meta, firstDrop)
		}
	}
}
