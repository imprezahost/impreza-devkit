package ingress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func code(err error) string {
	var v *ValidationError
	if errors.As(err, &v) {
		return v.Code
	}
	return ""
}

func calls(k *fakeKernel, substr string) int {
	n := 0
	for _, c := range k.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func TestDockerBridgesAreExactAndValidated(t *testing.T) {
	k := newKernel(true)
	k.networks = append(k.networks,
		"aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111|eth0 -j ACCEPT",
		"not-an-id|br9",
		"bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222|",
		defaultNet+"|docker0")
	got, err := DockerBridges(context.Background(), k.runner())
	if err != nil || strings.Join(got, ",") != "br-bbbbbbbbbbbb,br-fedcba987654,docker0" {
		t.Fatalf("bridges %v %v", got, err)
	}
	k.dockerDown = true
	if _, err := DockerBridges(context.Background(), k.runner()); err == nil {
		t.Fatal("docker down must be an error, not an empty list")
	}
}

func TestPublicInterfaceOnDockerBridgeIsNeverApplied(t *testing.T) {
	k := newKernel(true)
	k.public = []string{"br0"}
	k.networks = append(k.networks, "cccccccccccc3333333333333333333333333333333333333333333333333333|br0")
	m := manager(t, k, true)
	st, err := m.Update(context.Background(), pg(1, "192.0.2.0/24"))
	if err == nil || st.Enforced || st.Reason != ReasonPublicBridge {
		t.Fatalf("public bridge applied: %+v %v", st, err)
	}
	if calls(k, "-restore") != 0 || k.count("v4", Chain, "") != 0 {
		t.Fatal("rules were restored although the internet arrives on an exempted bridge")
	}
	if err := m.Reconcile(context.Background()); err == nil {
		if r, _ := m.Report(); len(r) != 1 || r[0].Enforced || r[0].Reason != ReasonPublicBridge {
			t.Fatalf("reconcile turned it green: %+v", r)
		}
	}
}

func TestBridgeNamedLikeDockerButNotListedIsNotExempted(t *testing.T) {
	k := newKernel(true)
	k.public = []string{"br0"} // a host bridge, not a Docker network
	m := manager(t, k, true)
	st, err := m.Update(context.Background(), pg(1, "192.0.2.0/24"))
	if err != nil || !st.Enforced {
		t.Fatalf("update: %+v %v", st, err)
	}
	for _, fam := range []string{"v4", "v6"} {
		if k.count(fam, Chain, "-i br0") != 0 || k.count(fam, HostChain, "-i br0") != 0 || k.count(fam, Chain, "br+") != 0 {
			t.Fatalf("%s: br0 exempted", fam)
		}
		if k.count(fam, Chain, "-i docker0 -j RETURN") != 1 || k.count(fam, HostChain, "-i br-fedcba987654 -j RETURN") != 1 {
			t.Fatalf("%s: docker bridges missing", fam)
		}
	}
}

func TestNewDockerNetworkIsExemptedOnReconcileAndDockerDownKeepsTheLast(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	if st, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil || !st.Enforced {
		t.Fatalf("update: %+v %v", st, err)
	}
	k.networks = append(k.networks, "dddddddddddd4444444444444444444444444444444444444444444444444444|")
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.count("v4", Chain, "-i br-dddddddddddd -j RETURN") != 1 {
		t.Fatal("a new Docker network was not exempted on reconcile")
	}
	before := k.mutations
	k.dockerDown = true
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.mutations != before || k.count("v4", Chain, "-i br-dddddddddddd -j RETURN") != 1 {
		t.Fatal("docker down changed the exemptions")
	}
	if r, _ := m.Report(); len(r) != 1 || !r[0].Enforced {
		t.Fatalf("report: %+v", r)
	}
}

func TestOpenInDisguiseIsRefused(t *testing.T) {
	for _, set := range [][]string{
		{"0.0.0.0/1", "128.0.0.0/1"},
		{"0.0.0.0/2", "64.0.0.0/2", "128.0.0.0/1"},
		{"128.0.0.0/1", "0.0.0.0/2", "64.0.0.0/3", "96.0.0.0/3"},
		{"::/1", "8000::/1"},
		{"::/2", "4000::/2", "8000::/1"},
		{"192.0.2.0/24", "::/1", "8000::/1"},
	} {
		if _, err := Normalize(pg(1, set...)); code(err) != "SOURCE_TOO_BROAD" || !strings.Contains(err.Error(), "open") {
			t.Fatalf("%v: %v", set, err)
		}
	}
	for _, set := range [][]string{
		{"0.0.0.0/1"},
		{"0.0.0.0/1", "128.0.0.0/2"},
		{"0.0.0.0/1", "::/1"}, // each family only half covered
		{"::/1", "8000::/2"},
	} {
		if _, err := Normalize(pg(1, set...)); err != nil {
			t.Fatalf("%v refused: %v", set, err)
		}
	}
}

func sources(n, base int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("10.%d.%d.0/24", (base+i)/256, (base+i)%256)
	}
	return out
}

func TestBoundaries(t *testing.T) {
	if _, err := Normalize(pg(1, sources(MaxSourcesPerRule, 0)...)); err != nil {
		t.Fatalf("%d sources refused: %v", MaxSourcesPerRule, err)
	}
	if _, err := Normalize(pg(1, sources(MaxSourcesPerRule+1, 0)...)); code(err) != "TOO_MANY_SOURCES" {
		t.Fatalf("%d sources: %v", MaxSourcesPerRule+1, err)
	}
	rules := func(n, perRule int) Policy {
		p := Policy{DeploymentID: "dpl_pg1", Revision: 1}
		for i := 0; i < n; i++ {
			per := perRule
			if per < 0 { // last rule gets the remainder of -per
				per = 1
				if i < n-1 {
					per = MaxSourcesPerRule
				}
			}
			p.Rules = append(p.Rules, Rule{Port: 30001 + i, Protocol: "tcp", Sources: sources(per, i*MaxSourcesPerRule)})
		}
		return p
	}
	if _, err := Normalize(rules(MaxRules, 1)); err != nil {
		t.Fatalf("%d rules refused: %v", MaxRules, err)
	}
	if _, err := Normalize(rules(MaxRules+1, 1)); code(err) != "TOO_MANY_RULES" {
		t.Fatalf("%d rules: %v", MaxRules+1, err)
	}
	if MaxSources != 8*MaxSourcesPerRule {
		t.Fatal("boundary fixture assumes 512 = 8 x 64")
	}
	if _, err := Normalize(rules(8, MaxSourcesPerRule)); err != nil {
		t.Fatalf("%d sources refused: %v", MaxSources, err)
	}
	if _, err := Normalize(rules(9, -1)); code(err) != "TOO_MANY_SOURCES" { // 8 x 64 + 1
		t.Fatalf("%d sources: %v", MaxSources+1, err)
	}
}

func TestMessagesNeverCarryTheSource(t *testing.T) {
	cases := map[string][]string{
		"198.51.100.7/24":         {"198.51.100.7/24"},
		"198.51.100.0/24 x":       {"198.51.100.0/24 x"},
		"::ffff:198.51.100.0/120": {"::ffff:198.51.100.0/120"},
		"::198.51.100.0/120":      {"::198.51.100.0/120"},
		"fe80::1:2/128%eth0":      {"fe80::1:2/128%eth0"},
		"dup":                     {"198.51.100.0/24", "198.51.100.0/24"},
		"open":                    {"0.0.0.0/1", "128.0.0.0/1"},
	}
	for name, set := range cases {
		p := Policy{DeploymentID: "dpl_pg1", Revision: 1, Rules: []Rule{
			{Port: 8080, Protocol: "tcp", Sources: []string{"192.0.2.0/24"}},
			{Port: 5432, Protocol: "tcp", Sources: set}}}
		_, err := Normalize(p)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		msg := err.Error()
		for _, leak := range []string{"198.51", "fe80", "128.0.0.0", "0.0.0.0/1"} {
			if strings.Contains(msg, leak) {
				t.Fatalf("%s: message carries the source: %q", name, msg)
			}
		}
		if !strings.Contains(msg, "rules[1].sources") {
			t.Fatalf("%s: message does not name the position: %q", name, msg)
		}
	}
}

func TestIPv4CompatibleIPv6IsCanonicalHex(t *testing.T) {
	for _, in := range []string{"::c000:200/120", "::C000:200/120", "0:0:0:0:0:0:c000:200/120"} {
		got, err := NormalizeSources([]string{in})
		if err != nil || len(got) != 1 || got[0] != "::c000:200/120" {
			t.Fatalf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"::192.0.2.0/120", "::ffff:192.0.2.0/120", "::ffff:c000:200/120"} {
		if _, err := NormalizeSources([]string{in}); code(err) != "INVALID_SOURCE" {
			t.Fatalf("%s: %v", in, err)
		}
	}
	p, err := Normalize(Policy{DeploymentID: "dpl_pg1", Revision: 2, Rules: []Rule{
		{Port: 5432, Protocol: "tcp", Sources: []string{"::C000:200/120", "192.0.2.0/24"}}}})
	if err != nil {
		t.Fatal(err)
	}
	// Shared vector with the control plane (lib/IngressRules.php).
	if got := PolicyFingerprint(p); got != fingerprintVectorCompat {
		t.Fatalf("fingerprint %s", got)
	}
}

func TestBootRestoreNeverAsksDocker(t *testing.T) {
	k := newKernel(true)
	m := manager(t, k, true)
	if st, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil || !st.Enforced {
		t.Fatalf("update: %+v %v", st, err)
	}
	// Boot: the chains are gone, and docker.socket accepts but the daemon is
	// ordered after the restore, so a docker call would hang.
	k2 := newKernel(true)
	k2.dockerHang = true
	m.Engine = Engine{Run: k2.runner()}
	m.Bridges = func(ctx context.Context) ([]string, error) { return DockerBridges(ctx, k2.runner()) }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Restore(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if calls(k2, "docker ") != 0 {
		t.Fatalf("restore asked Docker: %v", k2.calls)
	}
	for _, fam := range []string{"v4", "v6"} {
		if k2.count(fam, Chain, "-i docker0 -j RETURN") != 1 || k2.count(fam, Chain, "--ctorigdstport 5432 -j DROP") != 1 {
			t.Fatalf("%s: restore did not render the last applied set", fam)
		}
	}
}

func TestHangingDockerCostsOnlyTheBridgeTimeout(t *testing.T) {
	old := bridgeTimeout
	bridgeTimeout = 100 * time.Millisecond
	defer func() { bridgeTimeout = old }()
	k := newKernel(true)
	m := manager(t, k, true)
	if st, err := m.Update(context.Background(), pg(1, "192.0.2.0/24")); err != nil || !st.Enforced {
		t.Fatalf("update: %+v %v", st, err)
	}
	k.dockerHang = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	st, err := m.Update(ctx, pg(2, "192.0.2.0/24", "198.51.100.0/24"))
	if err != nil || !st.Enforced || time.Since(start) > 2*time.Second {
		t.Fatalf("update with a hanging Docker: %+v %v after %s", st, err, time.Since(start))
	}
	if k.count("v4", Chain, "-i docker0 -j RETURN") != 1 {
		t.Fatal("the last applied bridges were not kept")
	}
}
