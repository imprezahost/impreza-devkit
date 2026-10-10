package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The standby pair. A ready swap flips traffic by container
// lifecycle with the Caddyfile byte-identical — these tests pin the
// fragment shape, the upgrade, the refusal to retarget a pair, and the
// read-back that keeps a route update from downgrading the pair.

const standbyPairFragment = `# deployment dpl_pairaaaaaaaaaaaaaaaaaaaaaaa
standby-pair.invalid {
  reverse_proxy dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080
}
`

func standbyPairSpec() *SwapSpec {
	return &SwapSpec{
		Peer:        "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app-next:8080",
		Path:        "/healthz",
		StatusClass: "2xx",
	}
}

func TestStandbyPairRenderPairWithHealthCheck(t *testing.T) {
	got := renderFragment("dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", []Route{{
		Hostname:  "standby-pair.invalid",
		OnionAddr: "pairaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.onion",
		Upstream:  "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080",
		TLSMode:   "internal",
		Swap:      standbyPairSpec(),
	}})
	// Both origins carry the pair; the block is the exact shape a swap
	// relies on (first-policy preference + the active gate).
	wantBlock := "  reverse_proxy dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080 dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app-next:8080 {\n" +
		"    lb_policy first\n" +
		"    lb_try_duration 3s\n" +
		"    lb_try_interval 250ms\n" +
		"    health_uri /healthz\n" +
		"    health_status 2xx\n" +
		"    health_interval 500ms\n" +
		"    health_timeout 2s\n" +
		"  }\n"
	if strings.Count(got, wantBlock) != 2 {
		t.Fatalf("fragment does not carry the pair on both origins:\n%s", got)
	}
	// The single form renders exactly as before.
	plain := renderFragment("dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", []Route{{Hostname: "standby-pair.invalid", Upstream: "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080"}})
	if !strings.Contains(plain, "  reverse_proxy dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080\n") || strings.Contains(plain, "lb_policy") {
		t.Fatalf("single form changed:\n%s", plain)
	}
}

func TestStandbyPairValidateSwapRefusesMalformedSpecs(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir()}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	frag := filepath.Join(c.StateDir, "deployments", "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa.caddy")
	if err := os.WriteFile(frag, []byte(standbyPairFragment), 0600); err != nil {
		t.Fatal(err)
	}
	base := Route{Hostname: "standby-pair.invalid", Upstream: "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080"}
	for name, mutate := range map[string]func(*SwapSpec, *Route){
		"peer of another deployment": func(s *SwapSpec, _ *Route) { s.Peer = "dpl_other9999999999999999999999-app-next:8080" },
		"peer port mismatch":         func(s *SwapSpec, _ *Route) { s.Peer = "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app-next:8081" },
		"path traversal":             func(s *SwapSpec, _ *Route) { s.Path = "/../etc" },
		"relative path":              func(s *SwapSpec, _ *Route) { s.Path = "healthz" },
		"status class out of range":  func(s *SwapSpec, _ *Route) { s.StatusClass = "6xx" },
		"upstream not the app slot":  func(s *SwapSpec, r *Route) { r.Upstream = "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-web:8080" },
	} {
		r := base
		spec := standbyPairSpec()
		mutate(spec, &r)
		r.Swap = spec
		if err := c.ApplyDeploymentRoutes(t.Context(), "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", []Route{r}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A refusal leaves the existing fragment untouched.
	got, _ := os.ReadFile(frag)
	if string(got) != standbyPairFragment {
		t.Fatalf("fragment changed by a refusal:\n%s", got)
	}
}

func TestStandbyPairEnableSwapPeerUpgradesTheFragment(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir()}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	frag := filepath.Join(c.StateDir, "deployments", "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa.caddy")
	if err := os.WriteFile(frag, []byte(standbyPairFragment), 0600); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	c.switchReload = func(context.Context) error { reloads++; return nil }
	if err := c.EnableSwapPeer(t.Context(), "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", *standbyPairSpec()); err != nil {
		t.Fatal(err)
	}
	upgraded, _ := os.ReadFile(frag)
	if !strings.Contains(string(upgraded), "lb_policy first") || strings.Contains(string(upgraded), "\n  reverse_proxy dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080\n") {
		t.Fatalf("not upgraded:\n%s", upgraded)
	}
	if host, port, pair, err := c.UpstreamHost("dpl_pairaaaaaaaaaaaaaaaaaaaaaaa"); err != nil || host != "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app" || port != "8080" || !pair {
		t.Fatalf("upstream %s:%s pair=%v err=%v", host, port, pair, err)
	}
	// Idempotent: the pair form is left alone — no second write, no reload.
	before := string(upgraded)
	if err := c.EnableSwapPeer(t.Context(), "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", *standbyPairSpec()); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(frag)
	if string(again) != before || reloads != 1 {
		t.Fatalf("not idempotent (reloads=%d)", reloads)
	}
	// A rejected reload restores the exact previous bytes.
	c.switchReload = func(context.Context) error { return errors.New("caddy refused") }
	os.WriteFile(frag, []byte(standbyPairFragment), 0600)
	if err := c.EnableSwapPeer(t.Context(), "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", *standbyPairSpec()); err == nil {
		t.Fatal("upgrade survived a rejected reload")
	}
	restored, _ := os.ReadFile(frag)
	if string(restored) != standbyPairFragment {
		t.Fatalf("fragment not restored:\n%s", restored)
	}
}

func TestStandbyPairRetargetRefusesThePairForm(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir()}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	frag := filepath.Join(c.StateDir, "deployments", "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa.caddy")
	paired := renderFragment("dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", []Route{{Hostname: "standby-pair.invalid", Upstream: "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080", Swap: standbyPairSpec()}})
	if err := os.WriteFile(frag, []byte(paired), 0600); err != nil {
		t.Fatal(err)
	}
	c.switchReload = func(context.Context) error { return nil }
	err := c.RetargetUpstream(t.Context(), "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app-next")
	if !errors.Is(err, ErrRetargetPair) {
		t.Fatalf("retarget of a pair fragment: %v", err)
	}
	got, _ := os.ReadFile(frag)
	if string(got) != paired {
		t.Fatal("the pair fragment was rewritten")
	}
}

func TestStandbyPairSwapSpecOfReadsThePairBack(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir()}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	frag := filepath.Join(c.StateDir, "deployments", "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa.caddy")
	paired := renderFragment("dpl_pairaaaaaaaaaaaaaaaaaaaaaaa", []Route{{Hostname: "standby-pair.invalid", Upstream: "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080", Swap: standbyPairSpec()}})
	if err := os.WriteFile(frag, []byte(paired), 0600); err != nil {
		t.Fatal(err)
	}
	spec, err := c.SwapSpecOf("dpl_pairaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || spec == nil || *spec != *standbyPairSpec() {
		t.Fatalf("spec %+v err %v", spec, err)
	}
	// The legacy form reads back nil (nothing to carry over).
	if err := os.WriteFile(frag, []byte(standbyPairFragment), 0600); err != nil {
		t.Fatal(err)
	}
	spec, err = c.SwapSpecOf("dpl_pairaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || spec != nil {
		t.Fatalf("legacy read %+v err %v", spec, err)
	}
}

func TestStandbyPairLiveUpstreamPrefersTheAppSlot(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir()}
	both := `{"apps":{"http":{"servers":{"s0":{"routes":[{"handle":[{"upstreams":[{"dial":"dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app-next:8080"},{"dial":"dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app:8080"}]}]}]}}}}}`
	c.liveConfig = func(context.Context) ([]byte, error) { return []byte(both), nil }
	live, err := c.LiveUpstream(t.Context(), "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || live != "dpl_pairaaaaaaaaaaaaaaaaaaaaaaa-app" {
		t.Fatalf("live %s err %v", live, err)
	}
}
