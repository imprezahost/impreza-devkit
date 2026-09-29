package proxy

// The shared proxy was recreated on every deploy with a route. The
// check looked for the env-file among the container's bind mounts, where
// `--env-file` never appears (Docker copies the file into the container at
// creation), so EnsureRunning removed and relaunched impreza_caddy every
// time, and every app on the host lost HTTPS for a few seconds. These tests
// drive EnsureRunning against the docker double, with Docker's `--format`
// templates evaluated for real, and only use what the 0.6.23 agent already
// had, so the same file also runs against that agent, where it fails.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
)

func newProxyDouble(t *testing.T) (*Caddy, *dockertest.Double, *bytes.Buffer) {
	t.Helper()
	d := dockertest.Install(t, dockertest.State{Networks: []string{NetworkName},
		ImageEnv: map[string][]string{Image: {"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"CADDY_VERSION=v2"}}})
	logs := &bytes.Buffer{}
	c := New(t.TempDir(), slog.New(slog.NewTextHandler(logs, nil)))
	return c, d, logs
}

// proxyLaunches counts the `docker run` and `docker rm` calls aimed at the proxy.
func proxyLaunches(d *dockertest.Double) (runs, removals int) {
	for _, call := range d.Calls("run") {
		if strings.Contains(strings.Join(call.Args, " "), "--name "+ContainerName+" ") {
			runs++
		}
	}
	for _, call := range d.Calls("rm") {
		if call.Args[len(call.Args)-1] == ContainerName {
			removals++
		}
	}
	return runs, removals
}

func proxyContainer(t *testing.T, d *dockertest.Double) dockertest.Container {
	t.Helper()
	c, ok := d.Container(ContainerName)
	if !ok {
		t.Fatal("no proxy container")
	}
	return c
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		if k, v, _ := strings.Cut(entry, "="); k == key {
			return v
		}
	}
	return ""
}

func TestEnsureRunningKeepsAMatchingProxy(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret-a", "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	first := proxyContainer(t, d)
	// Two more deploys with a route, then the onion reconcile of a restart.
	for i := 0; i < 3; i++ {
		if err := c.EnsureRunning(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, removals := proxyLaunches(d)
	if runs != 1 || removals != 0 {
		t.Fatalf("a proxy matching this agent was recreated: %d launches, %d removals (the downtime on every deploy)", runs, removals)
	}
	if now := proxyContainer(t, d); now.ID != first.ID || now.State.Status != "running" {
		t.Fatalf("the serving proxy changed: %s/%s -> %s/%s", first.ID[:12], first.State.Status, now.ID[:12], now.State.Status)
	}
}

func TestEnsureRunningRecreatesForANewEnvironment(t *testing.T) {
	c, d, logs := newProxyDouble(t)
	ctx := context.Background()
	if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret-a", "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	first := proxyContainer(t, d)
	if got := envValue(first.Config.Env, ImprezaAgentSecretEnvVar); got != "proxy-secret-a" {
		t.Fatalf("the proxy did not start with the env-file: %q", got)
	}
	// Re-bootstrapped credentials: the file changes and the running proxy
	// is restarted, which keeps the environment Docker copied at creation.
	if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret-b", "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if got := envValue(proxyContainer(t, d).Config.Env, ImprezaAgentSecretEnvVar); got != "proxy-secret-a" {
		t.Fatalf("docker restart does not reload an env-file, the double must model that: %q", got)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	second := proxyContainer(t, d)
	if second.ID == first.ID || envValue(second.Config.Env, ImprezaAgentSecretEnvVar) != "proxy-secret-b" {
		t.Fatal("the new credentials never reached the proxy")
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if runs, _ := proxyLaunches(d); runs != 2 {
		t.Fatalf("expected exactly one recreation for the new environment, got %d launches", runs)
	}
	if strings.Contains(logs.String(), "proxy-secret") {
		t.Fatal("the proxy lifecycle logged a credential")
	}
}

// A proxy created by an older agent (the same image and mounts, no spec
// label) is recreated once, then kept.
func TestEnsureRunningReplacesAnOlderAgentsProxyOnce(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.SetImprezaCredentials(ctx, "agt_proxy", "proxy-secret-a", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	old := dockertest.Container{
		ID:    strings.Repeat("0a", 32),
		Name:  "/" + ContainerName,
		Image: "sha256:" + strings.Repeat("1b", 32),
		State: dockertest.ContainerState{Status: "running"},
		Config: dockertest.ContainerConfig{Image: Image, Labels: map[string]string{},
			Env: []string{"PATH=/usr/bin", ImprezaAgentIDEnvVar + "=agt_proxy", ImprezaAgentSecretEnvVar + "=proxy-secret-a", ImprezaAPIURLEnvVar + "="}},
		HostConfig: dockertest.HostConfig{Binds: []string{
			filepath.Join(c.StateDir, "Caddyfile") + ":/etc/caddy/Caddyfile:ro",
			filepath.Join(c.StateDir, "data") + ":/data",
			filepath.Join(c.StateDir, "config") + ":/config",
		}},
	}
	state := d.State()
	state.Containers = append(state.Containers, old)
	d.Save(state)
	for i := 0; i < 3; i++ {
		if err := c.EnsureRunning(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, removals := proxyLaunches(d)
	if runs != 1 || removals != 1 {
		t.Fatalf("an older agent's proxy must be replaced exactly once: %d launches, %d removals", runs, removals)
	}
	if proxyContainer(t, d).ID == old.ID {
		t.Fatal("the older agent's proxy was never replaced")
	}
}

func TestEnsureRunningStartsAStoppedMatchingProxy(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	state := d.State()
	for i := range state.Containers {
		if state.Containers[i].Name == "/"+ContainerName {
			state.Containers[i].State.Status = "exited"
		}
	}
	d.Save(state)
	first := proxyContainer(t, d)
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	now := proxyContainer(t, d)
	if runs, removals := proxyLaunches(d); runs != 1 || removals != 0 || now.ID != first.ID || now.State.Status != "running" {
		t.Fatalf("a stopped proxy of the same shape must be started, not recreated: %d launches, %d removals, %s", runs, removals, now.State.Status)
	}
}

// An env-file the Docker CLI would refuse must not cost the running proxy:
// removing it first would leave the host with no proxy at all.
func TestEnsureRunningKeepsTheProxyWhenTheEnvFileIsUnusable(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	first := proxyContainer(t, d)
	if err := os.WriteFile(filepath.Join(c.StateDir, "caddy.env"), []byte("IMPREZA_AGENT_ID=\xff\xfe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = c.EnsureRunning(ctx)
	now, ok := d.Container(ContainerName)
	if !ok || now.ID != first.ID || now.State.Status != "running" {
		t.Fatal("an unusable env-file took the serving proxy down")
	}
}
