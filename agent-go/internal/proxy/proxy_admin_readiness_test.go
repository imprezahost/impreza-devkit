package proxy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The actual route entry starts Caddy detached, then writes and reloads.
// The double rejects reload while its admin endpoint is still starting.
func TestColdProxyWaitsBeforeRouteReload(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	s := d.State()
	s.ProxyReadyAfter = 2
	d.Save(s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyDeploymentRoutes(ctx, "dpl_proxyreadiness", []Route{{Hostname: "app.invalid", TLSMode: "none", Upstream: "dpl_proxyreadiness-app:80"}}); err != nil {
		t.Fatalf("cold-start route failed: %v", err)
	}
	if d.State().ProxyReadinessProbes < 3 {
		t.Fatal("detached container was treated as an accepting admin listener")
	}
	if runs, removals := proxyLaunches(d); runs != 1 || removals != 0 {
		t.Fatal("readiness recreated the proxy")
	}
}

func TestAdminUnavailableFailsClosedWithinBoundedAdminBudget(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	s := d.State()
	s.ProxyNeverReady = true
	d.Save(s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err := c.EnsureRunning(ctx)
	if err == nil || !strings.Contains(err.Error(), "admin endpoint") {
		t.Fatalf("unavailable admin accepted: %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("probe exceeded its bounded admin startup budget")
	}
	for _, call := range d.Calls("exec") {
		if len(call.Args) > 3 && call.Args[2] == "caddy" && call.Args[3] == "reload" {
			t.Fatal("reload sent before readiness")
		}
	}
	if runs, removals := proxyLaunches(d); runs != 1 || removals != 0 {
		t.Fatal("unavailable admin restarted or removed proxy")
	}
}

func TestReadinessDoesNotRetryRejectedConfig(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx := context.Background()
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	s := d.State()
	s.ProxyRejectReload = true
	d.Save(s)
	if err := c.ApplyDeploymentRoutes(ctx, "dpl_proxyreadiness", []Route{{Hostname: "app.invalid", TLSMode: "none", Upstream: "dpl_proxyreadiness-app:80"}}); err == nil {
		t.Fatal("rejected config accepted")
	}
	reloads := 0
	for _, call := range d.Calls("exec") {
		if len(call.Args) > 3 && call.Args[2] == "caddy" && call.Args[3] == "reload" {
			reloads++
		}
	}
	if reloads != 1 {
		t.Fatalf("configuration rejection retried: %d", reloads)
	}
	if runs, removals := proxyLaunches(d); runs != 1 || removals != 0 {
		t.Fatal("configuration rejection recreated proxy")
	}
}

func TestReadinessHonorsCanceledCaller(t *testing.T) {
	c, d, _ := newProxyDouble(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := c.EnsureRunning(ctx); err == nil {
		t.Fatal("canceled caller accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled caller waited for startup budget")
	}
	for _, call := range d.Calls("exec") {
		if len(call.Args) > 3 && call.Args[2] == "caddy" && call.Args[3] == "reload" {
			t.Fatal("canceled caller sent reload")
		}
	}
}
