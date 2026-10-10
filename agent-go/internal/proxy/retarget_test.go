package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func retargetFixture(t *testing.T) (*Caddy, string, string, []byte) {
	t.Helper()
	c := &Caddy{StateDir: t.TempDir()}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	id := "dpl_" + strings.Repeat("a", 16)
	other := "dpl_" + strings.Repeat("b", 16)
	body := "# deployment " + id + "\n" +
		"app.example.test {\n  tls internal\n  reverse_proxy " + id + "-app:8080\n}\n\n" +
		"http://" + strings.Repeat("c", 56) + ".onion {\n  reverse_proxy " + id + "-app:8080\n}\n\n"
	if err := os.WriteFile(filepath.Join(c.StateDir, "deployments", id+".caddy"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	otherBody := []byte("other.example.test {\n  reverse_proxy " + other + "-app:8080\n}\n")
	if err := os.WriteFile(filepath.Join(c.StateDir, "deployments", other+".caddy"), otherBody, 0600); err != nil {
		t.Fatal(err)
	}
	return c, id, other, []byte(body)
}

func TestRetargetMovesOnlyTheUpstreamHost(t *testing.T) {
	c, id, other, before := retargetFixture(t)
	reloads := 0
	c.switchReload = func(context.Context) error { reloads++; return nil }
	if err := c.RetargetUpstream(t.Context(), id, id+"-app-next"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(c.StateDir, "deployments", id+".caddy"))
	want := strings.ReplaceAll(string(before), "reverse_proxy "+id+"-app:8080", "reverse_proxy "+id+"-app-next:8080")
	if string(got) != want || reloads != 1 {
		t.Fatalf("fragment=%q reloads=%d", got, reloads)
	}
	if host, port, pair, err := c.UpstreamHost(id); err != nil || host != id+"-app-next" || port != "8080" || pair {
		t.Fatalf("upstream %s:%s %v", host, port, err)
	}
	gotOther, _ := os.ReadFile(filepath.Join(c.StateDir, "deployments", other+".caddy"))
	if !strings.Contains(string(gotOther), other+"-app:8080") {
		t.Fatal("another deployment's fragment changed")
	}
	caddyfile, _ := os.ReadFile(filepath.Join(c.StateDir, "Caddyfile"))
	if !strings.Contains(string(caddyfile), id+"-app-next:8080") || strings.Contains(string(caddyfile), id+"-app:8080") {
		t.Fatal("Caddyfile not regenerated with the new upstream")
	}
	// Back, and idempotent on the current target.
	for i := 0; i < 2; i++ {
		if err := c.RetargetUpstream(t.Context(), id, id+"-app"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = os.ReadFile(filepath.Join(c.StateDir, "deployments", id+".caddy"))
	if string(got) != string(before) {
		t.Fatal("retarget back is not byte-exact")
	}
}

func TestRetargetRestoresOnRejectedReload(t *testing.T) {
	c, id, _, before := retargetFixture(t)
	calls := 0
	c.switchReload = func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("injected reload failure")
		}
		return ctx.Err()
	}
	if err := c.RetargetUpstream(t.Context(), id, id+"-app-next"); err == nil {
		t.Fatal("rejected reload accepted")
	}
	got, _ := os.ReadFile(filepath.Join(c.StateDir, "deployments", id+".caddy"))
	if string(got) != string(before) || calls != 2 {
		t.Fatalf("not restored (calls=%d)", calls)
	}
}

func TestRetargetRefusals(t *testing.T) {
	c, id, other, _ := retargetFixture(t)
	c.switchReload = func(context.Context) error { t.Fatal("reload on refusal"); return nil }
	for name, to := range map[string]string{
		"other deployment": other + "-app",
		"not an app name":  id + "-db",
		"injection":        id + "-app:9 {\n}",
	} {
		if err := c.RetargetUpstream(t.Context(), id, to); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	for name, body := range map[string]string{
		"two ports":       "a.test {\n  reverse_proxy " + id + "-app:8080\n}\n\nb.test {\n  reverse_proxy " + id + "-app:9090\n}\n",
		"foreign target":  "a.test {\n  reverse_proxy " + other + "-app:8080\n}\n",
		"no upstream":     "a.test {\n  respond 200\n}\n",
		"external target": "a.test {\n  reverse_proxy 10.0.0.1:8080\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(c.StateDir, "deployments", id+".caddy"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := c.RetargetUpstream(t.Context(), id, id+"-app-next"); !errors.Is(err, ErrRetargetShape) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRetargetBlockedByPendingHostnameSwitch(t *testing.T) {
	c, id, _, _ := retargetFixture(t)
	c.switchReload = func(context.Context) error { return nil }
	if err := c.beginRoutingSwitch(routingSwitchRecord{Version: 1, Hostname: "app.example.test", Source: id, Target: "dpl_" + strings.Repeat("b", 16)}); err != nil {
		t.Fatal(err)
	}
	if err := c.RetargetUpstream(t.Context(), id, id+"-app-next"); !errors.Is(err, ErrRoutingRecoveryRequired) {
		t.Fatalf("retarget during a pending hostname switch: %v", err)
	}
}

// LiveUpstream reads the configuration Caddy has loaded, which the fragment
// on disk can be ahead of. The shape is the one `/config/apps/http` returns
// for an agent route (measured on Caddy 2.11.4).
func TestLiveUpstreamReadsTheLoadedConfiguration(t *testing.T) {
	route := func(dial string) string {
		return `{"handle":[{"handler":"subroute","routes":[{"handle":[{"deployment":"dpl_aaaaaaaaaaaaaaaa","handler":"proxy_metrics"},{"handler":"reverse_proxy","upstreams":[{"dial":"` + dial + `"}]}]}]}],"match":[{"host":["app.example.test"]}],"terminal":true}`
	}
	config := func(routes ...string) string {
		return `{"grace_period":0,"servers":{"srv0":{"listen":[":443"],"routes":[` + strings.Join(routes, ",") + `]}}}`
	}
	other := route("dpl_aaaaaaaaaaaaaaaab-app:8080") // another deployment whose id extends this one
	for name, tc := range map[string]struct {
		raw  string
		err  error
		want string
		bad  string
	}{
		"on app":  {raw: config(route("dpl_aaaaaaaaaaaaaaaa-app:8080"), other), want: "dpl_aaaaaaaaaaaaaaaa-app"},
		"on next": {raw: config(route("dpl_aaaaaaaaaaaaaaaa-app-next:8080"), other), want: "dpl_aaaaaaaaaaaaaaaa-app-next"},
		// Both dials is the standby-pair form, not a disagreement —
		// the preferred (app) slot is what "live" means there.
		"both slots (the pair form)": {raw: config(route("dpl_aaaaaaaaaaaaaaaa-app:8080"), route("dpl_aaaaaaaaaaaaaaaa-app-next:8080")), want: "dpl_aaaaaaaaaaaaaaaa-app"},
		"no route":                   {raw: config(other), bad: "no route to this deployment"},
		"unreadable":                 {err: errors.New("exit status 1"), bad: "could not be read"},
		"not json":                   {raw: "<html>", bad: "not JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &Caddy{liveConfig: func(context.Context) ([]byte, error) { return []byte(tc.raw), tc.err }}
			got, err := c.LiveUpstream(context.Background(), "dpl_aaaaaaaaaaaaaaaa")
			if tc.bad != "" {
				if err == nil || !strings.Contains(err.Error(), tc.bad) {
					t.Fatalf("got %q, %v; want an error with %q", got, err, tc.bad)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := (&Caddy{}).LiveUpstream(context.Background(), "../x"); err == nil {
		t.Fatal("an invalid deployment identity was accepted")
	}
}
