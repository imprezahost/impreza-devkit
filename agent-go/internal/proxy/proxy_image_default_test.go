package proxy

// An image default was allowed in the proxy container by name only.
// A variable named like one (PATH, CADDY_VERSION; on the real image the
// XDG_* too) set to ANOTHER value through the env-file reached the proxy
// and, once dropped from the file, stayed: EnsureRunning kept the
// container and the removed value lingered in the environment of the proxy
// exposed to the internet. The defaults are now compared as NAME=value;
// HOSTNAME, set by the daemon to the container's own name, is the one entry
// allowed by name, and nothing tested it. These drive EnsureRunning against
// the docker double.

import (
	"context"
	"os"
	"strings"
	"testing"
)

func appendProxyEnvLine(t *testing.T, c *Caddy, line string) []byte {
	t.Helper()
	raw, err := os.ReadFile(c.envFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.envFilePath(), append(append([]byte{}, raw...), []byte(line+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestEnsureRunningDropsAnImageDefaultSetToAnotherValue(t *testing.T) {
	for _, tc := range []struct{ key, stale string }{
		{"PATH", "/opt/stale/bin"},
		{"CADDY_VERSION", "stale-version"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			c, d, _ := newProxyDouble(t)
			ctx := context.Background()
			if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret", "https://api.example.invalid"); err != nil {
				t.Fatal(err)
			}
			if err := c.EnsureRunning(ctx); err != nil {
				t.Fatal(err)
			}
			raw := appendProxyEnvLine(t, c, tc.key+"="+tc.stale)
			if err := c.EnsureRunning(ctx); err != nil {
				t.Fatal(err)
			}
			added := proxyContainer(t, d)
			if got := envValue(added.Config.Env, tc.key); got != tc.stale {
				t.Fatalf("setup: the override never reached the proxy (%q)", got)
			}
			if err := os.WriteFile(c.envFilePath(), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := c.EnsureRunning(ctx); err != nil {
				t.Fatal(err)
			}
			now := proxyContainer(t, d)
			if now.ID == added.ID {
				t.Fatalf("%s=%s left the env-file and the serving proxy was kept with it", tc.key, tc.stale)
			}
			if got := envValue(now.Config.Env, tc.key); got == tc.stale {
				t.Fatalf("the removed %s=%s is still in the proxy's environment", tc.key, tc.stale)
			}
			if err := c.EnsureRunning(ctx); err != nil {
				t.Fatal(err)
			}
			if runs, _ := proxyLaunches(d); runs != 3 {
				t.Fatalf("expected three launches (start, override, removal) and then a stable proxy, got %d", runs)
			}
		})
	}
}

// The image's own value set through the env-file and dropped again changes
// nothing in the container: no recreation, either way.
func TestEnsureRunningKeepsAnImageDefaultAtTheImagesValue(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	def := envValue(proxyContainer(t, d).Config.Env, "PATH")
	if def == "" {
		t.Fatal("setup: the proxy has no PATH")
	}
	raw := appendProxyEnvLine(t, c, "PATH="+def)
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.envFilePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if runs, _ := proxyLaunches(d); runs != 1 {
		t.Fatalf("the image's own PATH in and out of the env-file recreated the proxy: %d launches", runs)
	}
}

// The daemon names every container in its HOSTNAME. A proxy carrying it is
// kept, whatever the value; without the allowance it would be recreated on
// every deploy.
func TestEnsureRunningKeepsTheDaemonsHostname(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret", "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	state := d.State()
	for i := range state.Containers {
		if strings.TrimPrefix(state.Containers[i].Name, "/") == ContainerName {
			state.Containers[i].Config.Env = append(state.Containers[i].Config.Env, "HOSTNAME="+state.Containers[i].ID[:12])
		}
	}
	d.Save(state)
	for i := 0; i < 2; i++ {
		if err := c.EnsureRunning(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if runs, removals := proxyLaunches(d); runs != 1 || removals != 0 {
		t.Fatalf("the daemon's HOSTNAME made the proxy be recreated: %d launches, %d removals", runs, removals)
	}
}
