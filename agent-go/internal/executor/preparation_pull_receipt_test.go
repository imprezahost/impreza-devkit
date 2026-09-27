package executor

// The supervised pull's own budget expiry must reconcile
// as a defined, retryable failure. Before the fix, a receipt with
// completed=false parked the poll loop in "pending" forever and the
// resume path demanded manual review — freezing the whole host.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePullReceipt writes a worker receipt directly, standing in for a
// worker that ended at its budget.
func writePullReceipt(t *testing.T, d *Docker, w *PreparationWork, completed, success bool) {
	t.Helper()
	dir, err := d.workDirectory(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeWorkJSON(dir, "result.json", preparationWorkResult{
		Version: 1, ID: w.ID, RequestSHA256: w.RequestSHA256,
		Completed: completed, Success: success,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPullReceiptWithoutVerdictIsDefinedFailure(t *testing.T) {
	d, r, _ := workFixture(t, "pull", "exit 0")
	writePullReceipt(t, d, r.Work, false, false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID)
	if errors.Is(err, ErrPreparationPending) {
		t.Fatal("incomplete pull receipt still parks the poller in pending")
	}
	if err == nil || !strings.Contains(err.Error(), "can be retried") {
		t.Fatalf("missing defined retryable failure: %v", err)
	}
}

func TestPullFailedReceiptReconcilesAsAborted(t *testing.T) {
	d, r, _ := workFixture(t, "pull", "exit 0")
	writePullReceipt(t, d, r.Work, true, false)

	ready, err := d.CompletedPreparationWork(r, r.Work.CommandID)
	if err != nil {
		t.Fatalf("failed pull receipt demanded manual review: %v", err)
	}
	if ready == nil || ready.Phase != "aborted" {
		t.Fatalf("pull receipt did not reconcile as aborted: %+v", ready)
	}
	if !ready.Recoverable() {
		t.Fatal("aborted pull recovery is not recoverable")
	}
}

// A failed plain build reconciles like the pull: it only produced an image
// nothing uses yet, and the review it used to demand held every later
// command on the host. The controlled builder keeps review
// (TestControlledBuildWithoutVerdictKeepsReview).
func TestBuildFailedReceiptReconcilesAsAborted(t *testing.T) {
	d, r, _ := workFixture(t, "build", "exit 0")
	writePullReceipt(t, d, r.Work, true, false)

	ready, err := d.CompletedPreparationWork(r, r.Work.CommandID)
	if err != nil || ready == nil || ready.Phase != "aborted" {
		t.Fatalf("failed plain build receipt did not reconcile as the defined failure: %+v %v", ready, err)
	}
}

func TestWorkerLaunchUsesCeilingAsRuntimeMaxSec(t *testing.T) {
	if runtimeMax := workBudget("pull"); runtimeMax < composePullCeiling {
		t.Fatalf("systemd RuntimeMaxSec %s sits under the pull ceiling %s", runtimeMax, composePullCeiling)
	}
}

// withPullBudgetKnobs shrinks the budget knobs for the watchdog test.
func withPullBudgetKnobs(base, stall, ceiling, tick time.Duration) (restore func()) {
	origBase, origStall, origCeiling, origTick := composePullTimeout, pullStallWindow, composePullCeiling, pullTick
	composePullTimeout, pullStallWindow, composePullCeiling, pullTick = base, stall, ceiling, tick
	return func() {
		composePullTimeout, pullStallWindow, composePullCeiling, pullTick = origBase, origStall, origCeiling, origTick
	}
}

// TestPullWorkerRecordsBudgetReasonAsDefinedFailure runs a real worker
// against a fake docker that never produces progress: the watchdog must
// kill it, and the receipt must be completed=true (defined failure) with
// the budget reason in its retained output.
//
// The fake docker leaves a child holding the output pipes (`sleep 60 &`),
// exactly what a compose plugin does when the CLI dies. Without a
// process-group kill plus a bounded pipe wait, Run() only returns when
// the sleep ends — so this test also measures the wall clock: the worker
// must finish right after the budget fired, not when the child exits.
func TestPullWorkerRecordsBudgetReasonAsDefinedFailure(t *testing.T) {
	defer withPullBudgetKnobs(600*time.Millisecond, 800*time.Millisecond, 10*time.Second, 200*time.Millisecond)()

	d, r, _ := workFixture(t, "pull", "sleep 60 &\nwait")
	started := time.Now()
	if err := RunPreparationWorker(d.StateDir, r.Work.ID); err != nil {
		t.Fatalf("worker failed to record its receipt: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 30*time.Second {
		t.Fatalf("worker stayed stuck behind the plugin's pipes for %s (budget fired at ~0.6s; the child sleeps 60s)", elapsed)
	}
	dir, err := d.workDirectory(r.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt preparationWorkResult
	if _, err := readWorkJSON(filepath.Join(dir, "result.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Completed || receipt.Success {
		t.Fatalf("budget expiry must be a defined failure: %+v", receipt)
	}
	if !strings.Contains(receipt.Output, "supervised pull") || !strings.Contains(receipt.Output, "retried") {
		t.Fatalf("retained output missing the budget reason: %q", receipt.Output)
	}
}
