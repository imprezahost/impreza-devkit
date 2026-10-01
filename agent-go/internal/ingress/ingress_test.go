package ingress

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKernel is one table per family with built-in and user chains. Every
// mutation is counted; iptables-restore --noflush applies a whole
// transaction or nothing.
type fakeKernel struct {
	chains      map[string]map[string][][]string // family -> chain -> rules
	order       map[string][]string
	mutations   int
	failRestore map[string]bool // family -> fail the next restores
	noV6        bool
	calls       []string
	networks    []string // docker network inspect lines ("<id>|<bridge option>")
	dockerDown  bool
	dockerHang  bool
	public      []string // default-route interfaces
}

func newKernel(dockerUser bool) *fakeKernel {
	k := &fakeKernel{chains: map[string]map[string][][]string{}, order: map[string][]string{}, failRestore: map[string]bool{},
		networks: []string{defaultNet + "|docker0", appNet + "|<no value>"}, public: []string{"eth0"}}
	for _, fam := range []string{"v4", "v6"} {
		k.chains[fam] = map[string][][]string{"INPUT": nil, "FORWARD": nil, "OUTPUT": nil}
		k.order[fam] = []string{"INPUT", "FORWARD", "OUTPUT"}
		if dockerUser {
			k.addChain(fam, "DOCKER-USER")
			k.chains[fam]["DOCKER-USER"] = [][]string{{"-j", "RETURN"}}
		}
	}
	return k
}

func (k *fakeKernel) addChain(fam, c string) {
	if _, ok := k.chains[fam][c]; !ok {
		k.chains[fam][c] = nil
		k.order[fam] = append(k.order[fam], c)
	}
}

func builtin(c string) bool { return c == "INPUT" || c == "FORWARD" || c == "OUTPUT" }

func (k *fakeKernel) runner() Runner {
	return func(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
		k.calls = append(k.calls, bin+" "+strings.Join(args, " "))
		if bin == "docker" {
			if k.dockerHang {
				<-ctx.Done() // a daemon that never answers
				return nil, ctx.Err()
			}
			return k.docker(args)
		}
		fam := "v4"
		if strings.HasPrefix(bin, "ip6") {
			fam = "v6"
			if k.noV6 {
				return nil, errors.New("not found")
			}
		}
		if len(args) >= 2 && args[0] == "-w" {
			args = args[2:]
		}
		if strings.HasSuffix(bin, "-restore") {
			return nil, k.restore(fam, args, stdin)
		}
		return k.iptables(fam, args)
	}
}

const (
	defaultNet = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	appNet     = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func (k *fakeKernel) docker(args []string) ([]byte, error) {
	if k.dockerDown {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}
	switch {
	case len(args) > 2 && args[0] == "network" && args[1] == "ls":
		var ids []string
		for _, n := range k.networks {
			id, _, _ := strings.Cut(n, "|")
			ids = append(ids, id[:min(12, len(id))])
		}
		return []byte(strings.Join(ids, "\n") + "\n"), nil
	case len(args) > 3 && args[0] == "network" && args[1] == "inspect":
		return []byte(strings.Join(k.networks, "\n") + "\n"), nil
	}
	return nil, errors.New("unexpected docker call")
}

func (k *fakeKernel) restore(fam string, args []string, input string) error {
	if len(args) != 1 || args[0] != "--noflush" {
		return errors.New("restore without --noflush")
	}
	if k.failRestore[fam] {
		return errors.New("injected restore failure")
	}
	staged := map[string][][]string{}
	declared := []string{}
	lines := strings.Split(strings.TrimSpace(input), "\n")
	if len(lines) < 2 || lines[0] != "*filter" || lines[len(lines)-1] != "COMMIT" {
		return errors.New("malformed restore input")
	}
	for _, l := range lines[1 : len(lines)-1] {
		switch {
		case strings.HasPrefix(l, ":"):
			name := strings.Fields(l[1:])[0]
			if builtin(name) || name == "DOCKER-USER" {
				return errors.New("restore touched a foreign chain")
			}
			staged[name] = [][]string{}
			declared = append(declared, name)
		case strings.HasPrefix(l, "-A "):
			f := strings.Fields(l)
			if _, ok := staged[f[1]]; !ok {
				return errors.New("restore appended to an undeclared chain")
			}
			staged[f[1]] = append(staged[f[1]], f[2:])
		default:
			return errors.New("unexpected restore line: " + l)
		}
	}
	for _, c := range declared {
		k.addChain(fam, c)
		k.chains[fam][c] = staged[c]
	}
	k.mutations++
	return nil
}

func eq(a, b []string) bool { return strings.Join(a, " ") == strings.Join(b, " ") }

func (k *fakeKernel) iptables(fam string, args []string) ([]byte, error) {
	if len(args) < 2 {
		return nil, errors.New("bad args")
	}
	op, chain, rest := args[0], args[1], args[2:]
	rules, ok := k.chains[fam][chain]
	switch op {
	case "-S":
		if !ok {
			return nil, errors.New("no chain")
		}
		var b strings.Builder
		if builtin(chain) {
			b.WriteString("-P " + chain + " ACCEPT\n")
		} else {
			b.WriteString("-N " + chain + "\n")
		}
		for _, r := range rules {
			b.WriteString("-A " + chain + " " + strings.Join(r, " ") + "\n")
		}
		return []byte(b.String()), nil
	case "-N":
		if ok {
			return nil, errors.New("exists")
		}
		k.addChain(fam, chain)
		k.mutations++
		return nil, nil
	case "-C":
		for _, r := range rules {
			if eq(r, rest) {
				return nil, nil
			}
		}
		return nil, errors.New("no match")
	case "-D":
		for i, r := range rules {
			if eq(r, rest) {
				k.chains[fam][chain] = append(append([][]string{}, rules[:i]...), rules[i+1:]...)
				k.mutations++
				return nil, nil
			}
		}
		return nil, errors.New("no match")
	case "-I":
		if !ok || len(rest) < 1 || rest[0] != "1" {
			return nil, errors.New("bad insert")
		}
		k.chains[fam][chain] = append([][]string{rest[1:]}, rules...)
		k.mutations++
		return nil, nil
	case "-F":
		k.chains[fam][chain] = nil
		k.mutations++
		return nil, nil
	case "-X":
		if len(rules) != 0 {
			return nil, errors.New("not empty")
		}
		delete(k.chains[fam], chain)
		k.mutations++
		return nil, nil
	}
	return nil, errors.New("unsupported op " + op)
}

func (k *fakeKernel) first(fam, chain string) string {
	r := k.chains[fam][chain]
	if len(r) == 0 {
		return ""
	}
	return strings.Join(r[0], " ")
}

func (k *fakeKernel) count(fam, chain, substr string) int {
	n := 0
	for _, r := range k.chains[fam][chain] {
		if strings.Contains(strings.Join(r, " "), substr) {
			n++
		}
	}
	return n
}

func manager(t *testing.T, k *fakeKernel, globalV6 bool) *Manager {
	t.Helper()
	return &Manager{StateDir: t.TempDir(), Engine: Engine{Run: k.runner()},
		HasGlobalIPv6: func() bool { return globalV6 }, Now: func() time.Time { return time.Unix(1790000000, 0) },
		Bridges:          func(ctx context.Context) ([]string, error) { return DockerBridges(ctx, k.runner()) },
		PublicInterfaces: func() []string { return k.public }}
}

func pg(rev uint32, sources ...string) Policy {
	return Policy{DeploymentID: "dpl_pg1", Revision: rev, Rules: []Rule{{Port: 5432, Protocol: "tcp", Sources: sources}}}
}

func TestNormalizeRefusesHostileSources(t *testing.T) {
	bad := map[string]string{
		"198.51.100.7/24":                  "HOST_BITS_SET",
		"0.0.0.0/0":                        "SOURCE_TOO_BROAD",
		"::/0":                             "SOURCE_TOO_BROAD",
		"fe80::/64%eth0":                   "INVALID_SOURCE",
		"::ffff:192.0.2.0/120":             "INVALID_SOURCE",
		"192.0.2.0/24 -j ACCEPT":           "INVALID_SOURCE",
		"192.0.2.0/24\n-A INPUT -j ACCEPT": "INVALID_SOURCE",
		" 192.0.2.0/24":                    "INVALID_SOURCE",
		"$(reboot)":                        "INVALID_SOURCE",
		"192.0.2.1":                        "INVALID_SOURCE",
	}
	for src, code := range bad {
		_, err := Normalize(pg(1, src))
		var v *ValidationError
		if !errors.As(err, &v) || v.Code != code {
			t.Fatalf("%q: want %s, got %v", src, code, err)
		}
	}
	if _, err := Normalize(pg(1, "192.0.2.0/24", "192.0.2.0/24")); err == nil || err.(*ValidationError).Code != "DUPLICATE_SOURCE" {
		t.Fatalf("duplicate accepted: %v", err)
	}
	many := make([]string, MaxSourcesPerRule+1)
	for i := range many {
		many[i] = fmt.Sprintf("10.%d.0.0/16", i)
	}
	if _, err := Normalize(pg(1, many...)); err == nil || err.(*ValidationError).Code != "TOO_MANY_SOURCES" {
		t.Fatalf("too many accepted: %v", err)
	}
	for _, r := range []Rule{{Port: 22, Protocol: "tcp"}, {Port: 443, Protocol: "udp"}, {Port: 0, Protocol: "tcp"},
		{Port: 5432, Protocol: "sctp"}, {Port: 70000, Protocol: "tcp"}} {
		r.Sources = []string{"192.0.2.0/24"}
		if _, err := Normalize(Policy{DeploymentID: "dpl_pg1", Revision: 1, Rules: []Rule{r}}); err == nil {
			t.Fatalf("rule %+v accepted", r)
		}
	}
	if _, err := Normalize(Policy{DeploymentID: "dpl_../x", Revision: 1}); err == nil {
		t.Fatal("bad deployment id accepted")
	}
	if _, err := Normalize(Policy{DeploymentID: "dpl_pg1", Revision: 0}); err == nil {
		t.Fatal("revision 0 accepted")
	}
}

func TestNormalizeCanonicalOrder(t *testing.T) {
	p, err := Normalize(Policy{DeploymentID: "dpl_pg1", Revision: 3, Rules: []Rule{
		{Port: 21116, Protocol: "udp", Sources: []string{"2001:db8::/32", "198.51.100.0/24", "192.0.2.128/25"}},
		{Port: 21116, Protocol: "tcp", Sources: []string{"192.0.2.0/24"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Rules[0].Protocol != "tcp" || strings.Join(p.Rules[1].Sources, ",") != "192.0.2.128/25,198.51.100.0/24,2001:db8::/32" {
		t.Fatalf("not canonical: %+v", p.Rules)
	}
	// Shared vector with the control plane (lib/IngressRules.php).
	if got := PolicyFingerprint(p); got != fingerprintVector {
		t.Fatalf("fingerprint %s", got)
	}
}

func TestRulesPerFamilyAndOrder(t *testing.T) {
	p, _ := Normalize(pg(1, "192.0.2.0/24", "2001:db8::/32"))
	bridges := []string{"br-fedcba987654", "docker0"}
	fwd4, host4 := Rules(V4, []Policy{p}, bridges)
	fwd6, host6 := Rules(V6, []Policy{p}, bridges)
	if strings.Join(fwd4[0], " ") != "-m conntrack --ctstate ESTABLISHED,RELATED -j RETURN" ||
		strings.Join(host4[0], " ") != "-m conntrack --ctstate ESTABLISHED,RELATED -j RETURN" {
		t.Fatal("ESTABLISHED must return first")
	}
	joined := func(r [][]string) string {
		var s []string
		for _, x := range r {
			s = append(s, strings.Join(x, " "))
		}
		return strings.Join(s, "\n")
	}
	if !strings.Contains(joined(fwd4), "-p tcp -m conntrack --ctstate DNAT --ctdir ORIGINAL --ctorigdstport 5432 -s 192.0.2.0/24 -j RETURN") ||
		strings.Contains(joined(fwd4), "2001:db8") || !strings.Contains(joined(fwd4), "--ctorigdstport 5432 -j DROP") {
		t.Fatalf("v4 forward:\n%s", joined(fwd4))
	}
	if !strings.Contains(joined(host6), "-p tcp -m tcp --dport 5432 -s 2001:db8::/32 -j RETURN") ||
		strings.Contains(joined(host6), "192.0.2") || !strings.Contains(joined(host6), "--dport 5432 -j DROP") ||
		!strings.Contains(joined(fwd6), "--ctorigdstport 5432 -j DROP") {
		t.Fatalf("v6:\n%s\n%s", joined(fwd6), joined(host6))
	}
	if !strings.Contains(joined(host4), "-i lo -j RETURN") {
		t.Fatal("loopback must return in INPUT")
	}
	// Only the bridges Docker lists are exempted, never a wildcard.
	for _, r := range [][][]string{fwd4, host4, fwd6, host6} {
		j := joined(r)
		if strings.Contains(j, "br+") || !strings.Contains(j, "-i docker0 -j RETURN") || !strings.Contains(j, "-i br-fedcba987654 -j RETURN") {
			t.Fatalf("exemptions:\n%s", j)
		}
	}
	if f, _ := Rules(V4, []Policy{p}, nil); strings.Contains(joined(f), "-i ") {
		t.Fatalf("no bridge listed, none exempted:\n%s", joined(f))
	}
}

func TestUpdateAppliesBothFamiliesAtomically(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	st, err := m.Update(context.Background(), pg(1, "192.0.2.0/24", "2001:db8::/32"))
	if err != nil || !st.Enforced || st.Fingerprint == "" || st.Revision != 1 {
		t.Fatalf("update: %+v %v", st, err)
	}
	for _, fam := range []string{"v4", "v6"} {
		if k.first(fam, "DOCKER-USER") != "-j IMPREZA-INGRESS" || k.first(fam, "INPUT") != "-j IMPREZA-INGRESS-HOST" {
			t.Fatalf("%s jumps: %q %q", fam, k.first(fam, "DOCKER-USER"), k.first(fam, "INPUT"))
		}
		if k.count(fam, "IMPREZA-INGRESS", "--ctorigdstport 5432 -j DROP") != 1 || k.count(fam, "IMPREZA-INGRESS-HOST", "--dport 5432 -j DROP") != 1 {
			t.Fatalf("%s drop rules missing", fam)
		}
	}
	restores := 0
	for _, c := range k.calls {
		if strings.Contains(c, "-restore") {
			restores++
		}
		if strings.Contains(c, " -F IMPREZA") || strings.Contains(c, " -A IMPREZA") {
			t.Fatalf("non-atomic chain edit: %s", c)
		}
	}
	if restores != 2 {
		t.Fatalf("want one restore per family, got %d", restores)
	}
	before := k.mutations
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.mutations != before {
		t.Fatalf("idempotent reconcile mutated %d times", k.mutations-before)
	}
	// Same revision again is a no-op for the result.
	if st2, err := m.Update(context.Background(), pg(1, "2001:db8::/32", "192.0.2.0/24")); err != nil || st2.Fingerprint != st.Fingerprint {
		t.Fatalf("same revision: %+v %v", st2, err)
	}
}

func TestFailureKeepsLastGoodSetAndIsNeverGreen(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	if _, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil {
		t.Fatal(err)
	}
	good := k.count("v4", "IMPREZA-INGRESS", "192.0.2.0/24")
	k.failRestore["v4"] = true
	st, err := m.Update(context.Background(), pg(2, "198.51.100.0/24"))
	if err == nil || st.Enforced || st.Fingerprint != "" || st.Reason != ReasonApplyFailed || st.Revision != 2 {
		t.Fatalf("failure reported green: %+v %v", st, err)
	}
	if k.count("v4", "IMPREZA-INGRESS", "192.0.2.0/24") != good || k.count("v4", "IMPREZA-INGRESS", "198.51.100.0/24") != 0 {
		t.Fatal("a failed apply changed the live set")
	}
	// First restricted revision failing: never green either.
	k2 := newKernel(true)
	k2.failRestore["v4"] = true
	m2 := manager(t, k2, true)
	if st, err := m2.Update(context.Background(), pg(1, "192.0.2.0/24")); err == nil || st.Enforced {
		t.Fatalf("first restricted failure green: %+v", st)
	}
	// Recovery: the next reconcile retries and turns green.
	k.failRestore["v4"] = false
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep, _ := m.Report()
	if len(rep) != 1 || !rep[0].Enforced || rep[0].Revision != 2 || rep[0].Fingerprint == "" {
		t.Fatalf("not recovered: %+v", rep)
	}
}

func TestReconcileRestoresJumpOrderAndFlushedChains(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	if _, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil {
		t.Fatal(err)
	}
	// ufw enable/reload puts its jumps above ours in INPUT.
	k.chains["v4"]["INPUT"] = append([][]string{{"-j", "ufw-before-logging-input"}, {"-j", "ufw-before-input"}}, k.chains["v4"]["INPUT"]...)
	// A Docker restart recreates DOCKER-USER empty in v6.
	k.chains["v6"]["DOCKER-USER"] = [][]string{{"-j", "RETURN"}}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.first("v4", "INPUT") != "-m comment --comment impreza-ingress -j IMPREZA-INGRESS-HOST" || k.count("v4", "INPUT", "IMPREZA-INGRESS-HOST") != 1 {
		t.Fatalf("INPUT jump not first/unique: %v", k.chains["v4"]["INPUT"])
	}
	if k.count("v4", "INPUT", "ufw-before-input") != 1 {
		t.Fatal("foreign rules removed")
	}
	if k.first("v6", "DOCKER-USER") == "" || !strings.Contains(k.first("v6", "DOCKER-USER"), "IMPREZA-INGRESS") {
		t.Fatalf("v6 DOCKER-USER jump missing: %v", k.chains["v6"]["DOCKER-USER"])
	}
	// A flushed own chain is noticed and re-rendered.
	k.chains["v4"]["IMPREZA-INGRESS"] = nil
	if err := m.Reconcile(context.Background()); err != nil || k.count("v4", "IMPREZA-INGRESS", "-j DROP") != 1 {
		t.Fatalf("flushed chain not restored: %v", err)
	}
}

func TestRemoveLastAndResetLeaveNoOrphan(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	if _, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil {
		t.Fatal(err)
	}
	other := Policy{DeploymentID: "dpl_rd1", Revision: 4, Rules: []Rule{{Port: 21116, Protocol: "udp", Sources: []string{"198.51.100.0/24"}}}}
	if _, err := m.Update(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(context.Background(), "dpl_pg1"); err != nil {
		t.Fatal(err)
	}
	if k.count("v4", "IMPREZA-INGRESS", "5432") != 0 || k.count("v4", "IMPREZA-INGRESS", "21116") != 2 {
		t.Fatal("removing one deployment touched the other")
	}
	// Open (empty rules) for the last one removes chains and jumps.
	if _, err := m.Update(context.Background(), Policy{DeploymentID: "dpl_rd1", Revision: 5}); err != nil {
		t.Fatal(err)
	}
	for _, fam := range []string{"v4", "v6"} {
		if _, ok := k.chains[fam]["IMPREZA-INGRESS"]; ok || k.count(fam, "DOCKER-USER", "IMPREZA") != 0 || k.count(fam, "INPUT", "IMPREZA") != 0 {
			t.Fatalf("%s orphan after open: %v", fam, k.chains[fam])
		}
	}
	// Reset: chains gone, reported unenforced until a new revision.
	if _, err := m.Update(context.Background(), pg(2, "192.0.2.0/24")); err != nil {
		t.Fatal(err)
	}
	if err := m.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.chains["v4"]["IMPREZA-INGRESS-HOST"]; ok || k.count("v4", "INPUT", "IMPREZA") != 0 {
		t.Fatal("reset left chains")
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.chains["v4"]["IMPREZA-INGRESS"]; ok {
		t.Fatal("reconcile re-applied a locally reset allowlist")
	}
	rep, _ := m.Report()
	for _, r := range rep {
		if r.DeploymentID == "dpl_pg1" && (r.Enforced || r.Reason != ReasonResetLocally) {
			t.Fatalf("reset reported green: %+v", r)
		}
	}
	if st, err := m.Update(context.Background(), pg(3, "192.0.2.0/24")); err != nil || !st.Enforced {
		t.Fatalf("new revision after reset: %+v %v", st, err)
	}
}

func TestOlderRevisionIsIgnoredAndStateRestoredAtBoot(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	if _, err := m.Update(context.Background(), pg(7, "192.0.2.0/24")); err != nil {
		t.Fatal(err)
	}
	if st, err := m.Update(context.Background(), pg(6, "198.51.100.0/24")); err == nil || st.Revision != 7 {
		t.Fatalf("older revision applied: %+v %v", st, err)
	}
	// Boot: an empty kernel without DOCKER-USER (restore runs before docker).
	boot := newKernel(false)
	m.Engine = Engine{Run: boot.runner()}
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if boot.first("v4", "DOCKER-USER") != "-j IMPREZA-INGRESS" || boot.count("v4", "IMPREZA-INGRESS", "192.0.2.0/24") != 1 {
		t.Fatalf("not restored: %v", boot.chains["v4"])
	}
}

func TestIPv6WithoutIp6tables(t *testing.T) {
	k := newKernel(true)
	k.noV6 = true
	m := manager(t, k, true)
	if st, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err == nil || st.Enforced || st.Reason != ReasonIPv6 {
		t.Fatalf("v6 host without ip6tables green: %+v %v", st, err)
	}
	m2 := manager(t, k, false)
	if st, err := m2.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil || !st.Enforced {
		t.Fatalf("v4-only host: %+v %v", st, err)
	}
}

func TestCorruptStateIsRefusedButResetStillWorks(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	if _, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(m.StateDir, stateFile))
	hostile := strings.Replace(string(raw), "192.0.2.0/24", "0.0.0.0/0", 1)
	if err := os.WriteFile(filepath.Join(m.StateDir, stateFile), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(context.Background()); err == nil {
		t.Fatal("hostile state applied")
	}
	if err := m.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.chains["v4"]["IMPREZA-INGRESS"]; ok {
		t.Fatal("reset with corrupt state left chains")
	}
}
