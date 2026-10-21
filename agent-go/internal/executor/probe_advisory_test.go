package executor

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// the settle-note lines that carry the public HTTPS probe's outcome.
// The advisory policy is unchanged — a failed probe is not a failed install —
// but the outcome may no longer be discarded: success must never read as
// "public HTTPS verified" when the probe did not pass. No network: the
// probeHTTPS seam pins the outcome.
func TestProbeAndAnnotate(t *testing.T) {
	orig := probeHTTPS
	t.Cleanup(func() { probeHTTPS = orig })

	calls := 0
	pin := func(ok bool) {
		calls = 0
		probeHTTPS = func(ctx context.Context, host string, log *slog.Logger) bool {
			calls++
			return ok
		}
	}
	run := func() string {
		return probeAndAnnotate(context.Background(), "app-abc.imprezaapps.com", slog.Default())
	}

	pin(true)
	note := run()
	if !strings.Contains(note, "public HTTPS confirmed on app-abc.imprezaapps.com") ||
		strings.Contains(note, "not confirmed") {
		t.Fatalf("probe passed: note must confirm the host and never say 'not confirmed'; got %q", note)
	}
	if calls != 1 {
		t.Fatalf("probe passed: the probe must run exactly once; ran %d", calls)
	}

	pin(false)
	note = run()
	if !strings.Contains(note, "public HTTPS not confirmed on app-abc.imprezaapps.com") ||
		!strings.Contains(note, "advisory") || !strings.Contains(note, "deployment finished") {
		t.Fatalf("probe failed: note must say not confirmed + advisory + deployment finished; got %q", note)
	}
	if calls != 1 {
		t.Fatalf("probe failed: the probe must run exactly once; ran %d", calls)
	}

	// The discriminator the base lacked: the two outcomes cannot collapse
	// into one string (a discarded result would).
	pin(true)
	passed := run()
	pin(false)
	failed := run()
	if passed == failed {
		t.Fatalf("discard mutant: pass and fail produced the same note %q", passed)
	}

	// Onion-only / no clearnet route: nothing is claimed, and the probe is
	// not even attempted (today's behavior preserved).
	calls = 0
	if got := probeAndAnnotate(context.Background(), "", slog.Default()); got != "" || calls != 0 {
		t.Fatalf("onion-only: expected no note and no probe; got %q, calls=%d", got, calls)
	}
}
