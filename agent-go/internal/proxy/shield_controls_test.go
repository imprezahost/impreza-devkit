package proxy

import (
	"context"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestShieldControlsActualRouteWrite(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir(), switchReload: func(context.Context) error { return nil }}
	v := 30
	difficulty := 2
	deadline := time.Now().Unix() + 3600
	controls := &sdkclient.ShieldControls{PowPaths: []string{"/admin/"}, RateLimitRPM: &v, PowDifficulty: &difficulty, TrustedSources: []string{"192.0.2.0/24"}, AttackExpiresAt: deadline}
	route := Route{Hostname: "controls.test", OnionAddr: "test.onion", TLSMode: "none", Upstream: "app:80", Shield: &ShieldConfig{Profile: "hardened", Mode: "enforce", Protocol: sdkclient.ShieldV2Protocol, Controls: controls}}
	if err := c.ApplyDeploymentRoutes(context.Background(), "dpl_controls", []Route{route}); err != nil {
		t.Fatal(err)
	}
	fragment := filepath.Join(c.StateDir, "deployments", "dpl_controls.caddy")
	raw, err := os.ReadFile(fragment)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(raw), "http://test.onion {")
	if len(parts) != 2 {
		t.Fatal("missing onion block")
	}
	if !strings.Contains(parts[0], "trusted_source 192.0.2.0/24") || strings.Contains(parts[1], "trusted_source") {
		t.Fatal("trusted ranges omitted from clearnet or exposed to onion")
	}
	for _, wanted := range []string{"SecRuleEngine On", "path_prefix /admin/", "limit 30", "difficulty 2", "max_difficulty 2", "adaptive_threshold 0", "attack_until "} {
		if !strings.Contains(string(raw), wanted) {
			t.Fatalf("missing %s", wanted)
		}
	}
	for _, file := range []string{fragment, filepath.Join(c.StateDir, "Caddyfile")} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("policy file mode %o", info.Mode().Perm())
		}
	}
	controls.PowPaths = []string{"/admin\n}", "/"}
	if err := c.ApplyDeploymentRoutes(context.Background(), "dpl_controls", []Route{route}); err == nil {
		t.Fatal("injection accepted by actual ApplyDeploymentRoutes")
	}
	unchanged, _ := os.ReadFile(fragment)
	if string(unchanged) != string(raw) {
		t.Fatal("rejected input modified persisted policy")
	}
	controls.PowPaths = []string{"/admin/"}
	route.Shield.Profile = "standard"
	route.Shield.Mode = "audit"
	if err := c.ApplyDeploymentRoutes(context.Background(), "dpl_controls", []Route{route}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(fragment)
	if strings.Contains(string(raw), "SecRuleEngine On") || !strings.Contains(string(raw), "SecRuleEngine DetectionOnly") || !strings.Contains(string(raw), "baseline_disabled") {
		t.Fatal("attack changed WAF or did not preserve base disabled PoW")
	}
	route.Shield.Profile = "off"
	if err := c.ApplyDeploymentRoutes(context.Background(), "dpl_controls", []Route{route}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(fragment)
	if strings.Contains(string(raw), "coraza_waf") || !strings.Contains(string(raw), "baseline_disabled") {
		t.Fatal("attack enabled previously disabled WAF")
	}
}
