package executor

// Companion of preparation_wait_liveness_test.go: these tests shrink the
// no-receipt ceiling and the liveness interval (buildNoReceiptCeiling,
// preparationLivenessInterval).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withLivenessKnobs(interval, buildCeiling time.Duration) (restore func()) {
	origInterval, origCeiling := preparationLivenessInterval, buildNoReceiptCeiling
	preparationLivenessInterval, buildNoReceiptCeiling = interval, buildCeiling
	return func() { preparationLivenessInterval, buildNoReceiptCeiling = origInterval, origCeiling }
}

// landReceiptLater writes a success receipt from outside the test goroutine
// (no t.Fatal there): the worker finishing while the wait is in progress.
func landReceiptLater(d *Docker, w *PreparationWork, after time.Duration) {
	go func() {
		time.Sleep(after)
		if dir, err := d.workDirectory(w.ID); err == nil {
			_ = writeWorkJSON(dir, "result.json", preparationWorkResult{Version: 1, ID: w.ID, RequestSHA256: w.RequestSHA256, Completed: true, Success: true, Output: "landed"})
		}
	}()
}

// A live build worker that never writes a receipt: the wait ends at the
// build ceiling (past RuntimeMaxSec in production) as pending, so the
// recovery path keeps the build in review — and the unit is not stopped:
// what a build leaves behind is for the review.
func TestPreparationWaitLiveBuildWithoutReceiptEndsAtCeiling(t *testing.T) {
	defer withLivenessKnobs(50*time.Millisecond, 1500*time.Millisecond)()
	d, r, bin := workFixture(t, "build", "exit 0")
	setSystemctl(t, bin, "echo \"$*\" >> '"+bin+"/systemctl.log'\nif [ \"$1\" = show ]; then echo active; fi")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPreparationPending) || !strings.Contains(err.Error(), "past its unit deadline") {
		t.Fatalf("a live build without a receipt must end pending at the ceiling: %v", err)
	}
	if elapsed < 1400*time.Millisecond || elapsed > 10*time.Second {
		t.Fatalf("the build wait ended after %s; the ceiling is 1.5 s", elapsed)
	}
	log, _ := os.ReadFile(filepath.Join(bin, "systemctl.log"))
	if strings.Contains(string(log), "stop ") {
		t.Fatalf("the build ceiling must not stop the unit; systemctl log: %q", log)
	}
}

// The production ceiling sits past the unit's RuntimeMaxSec, so systemd
// has killed the worker before the wait gives up on a receipt.
func TestBuildNoReceiptCeilingPastRuntimeMaxSec(t *testing.T) {
	if runtimeMax := workBudget("build") + 60*time.Second; buildNoReceiptCeiling <= runtimeMax {
		t.Fatalf("build no-receipt ceiling %s does not sit past RuntimeMaxSec %s", buildNoReceiptCeiling, runtimeMax)
	}
	if preparationUnitStopGrace >= preparationUnitStopTimeout {
		t.Fatalf("TimeoutStopSec %s does not fit the %s stop confirmation window", preparationUnitStopGrace, preparationUnitStopTimeout)
	}
}

// One stopped reading is not death: systemd can answer that transiently.
// Readings that disagree keep the wait going until the receipt lands.
func TestPreparationWaitDoesNotBelieveASingleDeadReading(t *testing.T) {
	defer withLivenessKnobs(100*time.Millisecond, buildNoReceiptCeiling)()
	d, r, bin := workFixture(t, "pull", "exit 0")
	counter := filepath.Join(bin, "probes")
	setSystemctl(t, bin, "n=$(cat '"+counter+"' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '"+counter+"'\nif [ $((n % 2)) -eq 1 ]; then echo inactive; else echo active; fi")
	landReceiptLater(d, r.Work, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID); err != nil || string(out) != "landed" {
		t.Fatalf("alternating stopped readings ended the wait: %q %v", out, err)
	}
}

// A state systemd cannot report is not a dead worker.
func TestPreparationWaitTreatsUnreadableStateAsUnknown(t *testing.T) {
	defer withLivenessKnobs(100*time.Millisecond, buildNoReceiptCeiling)()
	d, r, bin := workFixture(t, "build", "exit 0")
	setSystemctl(t, bin, "exit 1")
	landReceiptLater(d, r.Work, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID); err != nil || string(out) != "landed" {
		t.Fatalf("an unreadable unit state ended the wait: %q %v", out, err)
	}
}

// The worker writes its receipt just before it exits: a stopped unit must
// not win over a receipt that landed between the two reads.
func TestPreparationWaitRereadsTheReceiptBeforeBelievingDeath(t *testing.T) {
	defer withLivenessKnobs(100*time.Millisecond, buildNoReceiptCeiling)()
	d, r, bin := workFixture(t, "pull", "exit 0")
	dir, err := d.workDirectory(r.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(preparationWorkResult{Version: 1, ID: r.Work.ID, RequestSHA256: r.Work.RequestSHA256, Completed: true, Success: true, Output: "landed"})
	staged := filepath.Join(bin, "receipt.json")
	if err := os.WriteFile(staged, receipt, 0o600); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(bin, "probes")
	result := filepath.Join(dir, "result.json")
	// The second reading reports the unit stopped, and the receipt lands in
	// that same instant.
	setSystemctl(t, bin, "n=$(cat '"+counter+"' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '"+counter+"'\n"+
		"if [ $n -ge 2 ]; then cp '"+staged+"' '"+result+"'; chmod 600 '"+result+"'; fi\necho inactive")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID); err != nil || string(out) != "landed" {
		t.Fatalf("the receipt that landed with the stop was lost: %q %v", out, err)
	}
}
