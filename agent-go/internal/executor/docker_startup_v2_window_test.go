package executor

// Coverage for the v2 settle window, deterministic by construction: the
// window logic runs on a virtual clock with scripted samples (the
// tracker), so no test depends on sample phase or real-time drift. These
// cannot exist on the pre-fix code (the tracker is the fix); their
// controls are mutation runs of the tracker. The behavior tests that
// carry the base control live in docker_startup_v2_settle_behavior_test.go.

import (
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func v2Sample(restarts int) []containerState {
	return []containerState{{Name: "web", Status: "running", Restarts: restarts}}
}

// TestV2WindowFullWindowSinceFirstSample: the window opens at the first
// all-OK sample and closes exactly one window later — not one sample
// count earlier (a sample-count form of window/interval observes
// window-interval seconds).
func TestV2WindowFullWindowSinceFirstSample(t *testing.T) {
	const window = 15 * time.Second
	var w v2WindowTracker
	w.observe(v2Sample(0), map[string]int{"web": 0}, time.Unix(1000, 0))
	for step := time.Duration(0); step < window; step += time.Second {
		if w.ready(time.Unix(1000, 0).Add(step), window) {
			t.Fatalf("ready %s into the window", step)
		}
	}
	if !w.ready(time.Unix(1000, 0).Add(window), window) {
		t.Fatal("not ready a full window after the first sample")
	}
}

// TestV2WindowSingleRestartThenStable: a service that restarts once and
// then stays up becomes ready one window after the restart. A restart the
// window has re-armed for is absorbed; re-arming on every sample (the
// fixed-baseline form) never lets a healthy deploy through.
func TestV2WindowSingleRestartThenStable(t *testing.T) {
	const window = 15 * time.Second
	var w v2WindowTracker
	w.observe(v2Sample(0), map[string]int{"web": 0}, time.Unix(2000, 0))                    // t=0 opens
	w.observe(v2Sample(1), map[string]int{"web": 0}, time.Unix(2000, 0).Add(4*time.Second)) // the restart, seen at t=4
	for step := 4 * time.Second; step < 4*time.Second+window; step += time.Second {
		w.observe(v2Sample(1), map[string]int{"web": 0}, time.Unix(2000, 0).Add(step))
		if w.ready(time.Unix(2000, 0).Add(step), window) {
			t.Fatalf("ready %s after the restart, before the window", step-4*time.Second)
		}
	}
	w.observe(v2Sample(1), map[string]int{"web": 0}, time.Unix(2000, 0).Add(4*time.Second+window))
	if !w.ready(time.Unix(2000, 0).Add(4*time.Second+window), window) {
		t.Fatal("a single restart followed by stability never became ready")
	}
}

// TestV2WindowSlowCrasherNeverReady: a restart every ~10 s — under the
// fast-path thresholds, always sampled as running — keeps re-arming the
// window and never becomes ready. A window that only opens once (the
// re-arm mutation) calls it ready after the first window.
func TestV2WindowSlowCrasherNeverReady(t *testing.T) {
	const window = 15 * time.Second
	var w v2WindowTracker
	now := time.Unix(3000, 0)
	for i := 0; i < 7; i++ {
		w.observe(v2Sample(i), map[string]int{"web": 0}, now)
		if w.ready(now, window) && i > 0 {
			t.Fatalf("a service restarting every 10 s became ready at sample %d", i)
		}
		now = now.Add(10 * time.Second)
	}
}

// TestV2WindowAppliesToMixedStacks: the window gates any v2 stack where
// some service has no healthcheck — and nothing else.
func TestV2WindowAppliesToMixedStacks(t *testing.T) {
	noCheck := []containerState{{Name: "web", Status: "running"}}
	mixed := []containerState{{Name: "web", Status: "running", Health: "healthy"}, {Name: "side", Status: "running"}}
	all := []containerState{{Name: "web", Status: "running", Health: "healthy"}}
	v2 := startupPolicy{RequireHealthy: true, V2: true}
	v1 := startupPolicy{RequireHealthy: true}
	if !v2WindowApplies(v2, noCheck) || !v2WindowApplies(v2, mixed) {
		t.Fatal("the window must gate stacks with an unchecked service")
	}
	if v2WindowApplies(v2, all) || v2WindowApplies(v1, noCheck) {
		t.Fatal("the window must not gate checked stacks or the v1 policy")
	}
}

// TestStartupPolicyRefusesUnknownProtocol: an unknown protocol is refused
// by name; v1 and v2 resolve.
func TestStartupPolicyRefusesUnknownProtocol(t *testing.T) {
	for _, name := range []string{"startup-health-v3", "Startup-Health-V2", "v2", "startup-health-v1 "} {
		if _, err := resolveStartupPolicy(&sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: 60, Protocol: name}); err == nil {
			t.Fatalf("unknown startup protocol accepted: %q", name)
		}
	}
	if p, err := resolveStartupPolicy(&sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: 60, Protocol: "startup-health-v2"}); err != nil || !p.V2 {
		t.Fatalf("v2 not resolved: %+v %v", p, err)
	}
}
