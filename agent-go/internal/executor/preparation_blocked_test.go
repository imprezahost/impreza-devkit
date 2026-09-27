package executor

// onion/data_dir deployments keep explicit reconciliation of
// the container replacement (Blocked), but the pull/build supervision must
// still run for them — the legacy phase value "blocked" disabled it, and
// an unsupervised stuck pull then held the whole poll queue behind a flat
// 45-minute deadline (the Tor Hosting case).

import (
	"strings"
	"testing"
)

func blockedFixture(t *testing.T) *PreparationRecovery {
	t.Helper()
	r := recoveryFixture()
	r.Blocked = true
	return r
}

func TestPreparationBlockedJournalValidates(t *testing.T) {
	work := &PreparationWork{ID: strings.Repeat("a", 32), Step: "pull", CommandID: "cmd_test", RequestSHA256: strings.Repeat("b", 64)}
	for _, phase := range []string{"busy", "ready", "aborted"} {
		r := blockedFixture(t)
		r.Phase = phase
		r.Work = work
		if err := r.Validate(); err != nil {
			t.Fatalf("blocked deployment with supervised pull must validate (phase %s): %v", phase, err)
		}
	}
	r := blockedFixture(t)
	r.Phase = "replacing"
	r.Work = nil
	if err := r.Validate(); err != nil {
		t.Fatalf("blocked deployment at replacement must validate: %v", err)
	}
	// The legacy phase value still loads (journals written before the flag
	// existed) and stays out of any worker binding.
	legacy := recoveryFixture()
	legacy.Phase = "blocked"
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy blocked checkpoint no longer validates: %v", err)
	}
	legacy.Work = work
	if legacy.Validate() == nil {
		t.Fatal("legacy blocked phase accepted a worker binding")
	}
}

func TestPreparationBlockedNeverAutoReconciles(t *testing.T) {
	work := &PreparationWork{ID: strings.Repeat("a", 32), Step: "pull", CommandID: "cmd_test", RequestSHA256: strings.Repeat("b", 64)}
	for _, phase := range []string{"ready", "aborted"} {
		r := blockedFixture(t)
		r.Phase = phase
		if phase == "aborted" {
			r.Work = work
		}
		if r.Recoverable() {
			t.Fatalf("blocked deployment auto-reconciles at phase %s; onion/data_dir interruption must keep explicit reconciliation", phase)
		}
	}
	// The same phases without the flag stay auto-recoverable.
	for _, phase := range []string{"ready", "aborted"} {
		r := recoveryFixture()
		r.Phase = phase
		if phase == "aborted" {
			r.Work = work
		}
		if !r.Recoverable() {
			t.Fatalf("unblocked deployment stopped being recoverable at phase %s", phase)
		}
	}
}
