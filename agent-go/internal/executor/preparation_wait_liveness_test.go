package executor

// The supervised wait moved to the deploy's context,
// which has no deadline (the poller runs Execute on its own loop context):
// a build worker that died without a receipt — OOM, a failed start, a full
// disk at the receipt write — kept the waiter, and with it the agent's
// whole command queue (onion revocations, uninstalls, updates), waiting
// forever; a pull worker dead in its first second held the queue for the
// 45-minute ceiling. The wait now reads the unit's state: dead without a
// receipt on a second reading ends the wait (pull: the defined, retryable
// failure; build: pending, so the recovery path keeps its review).
//
// The recovery path and the pull stop share the same rules: a state
// systemd cannot report proves nothing, a stopping unit is alive, and an
// unconfirmed stop goes back to recovery instead of failing the deploy
// with the unit maybe alive. The worker unit stops within the confirmation
// window (TimeoutStopSec).
//
// These tests use only symbols the base commit has, so they run as its
// negative control too.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setSystemctl(t *testing.T, bin, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationWaitEndsWhenPullWorkerDiesWithoutReceipt(t *testing.T) {
	d, r, bin := workFixture(t, "pull", "exit 0")
	setSystemctl(t, bin, "echo inactive")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID)
	if errors.Is(err, ErrPreparationPending) || err == nil || !strings.Contains(err.Error(), "can be retried") {
		t.Fatalf("a dead pull worker must end in the defined, retryable failure: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("a dead pull worker held the wait for %s", elapsed)
	}
}

func TestPreparationWaitEndsWhenBuildWorkerDiesWithoutReceipt(t *testing.T) {
	d, r, bin := workFixture(t, "build", "exit 0")
	setSystemctl(t, bin, "echo failed")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPreparationPending) {
		t.Fatalf("a dead build worker must go back to recovery as pending: %v", err)
	}
	if elapsed > 20*time.Second || ctx.Err() != nil {
		t.Fatalf("a dead build worker held the wait for %s (it only ended with the context)", elapsed)
	}
}

// The worker unit must stop inside the confirmation window: with systemd's
// default TimeoutStopSec (90 s), a worker ignoring SIGTERM always outlived
// the 30 s window and ended in review.
func TestPreparationUnitStopGraceFitsTheConfirmationWindow(t *testing.T) {
	d, r, bin := workFixture(t, "pull", "exit 0")
	if err := os.WriteFile(filepath.Join(bin, "systemd-run"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+bin+"/systemd-run.args\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.launchPreparationWork(context.Background(), r.Work); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(bin, "systemd-run.args"))
	grace := -1
	for _, arg := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(arg, "--property=TimeoutStopSec="); ok {
			grace, _ = strconv.Atoi(value)
		}
	}
	if grace <= 0 || time.Duration(grace)*time.Second >= preparationUnitStopTimeout {
		t.Fatalf("worker unit TimeoutStopSec %d s does not fit the %s stop confirmation window; args:\n%s", grace, preparationUnitStopTimeout, raw)
	}
}

// A pull unit that cannot be confirmed stopped goes back to the recovery
// path, which checks it again — never a failed deploy whose configuration
// is restored and whose journal is cleared with the unit maybe alive.
func TestPullNoReceiptUnconfirmedStopStaysPending(t *testing.T) {
	defer withPullBudgetKnobs(300*time.Millisecond, 400*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond)()
	orig := preparationUnitStopTimeout
	preparationUnitStopTimeout = 900 * time.Millisecond
	defer func() { preparationUnitStopTimeout = orig }()
	d, r, bin := workFixture(t, "pull", "exit 0")
	setSystemctl(t, bin, "if [ \"$1\" = show ]; then echo active; fi")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID)
	if !errors.Is(err, ErrPreparationPending) || !strings.Contains(err.Error(), "review required") {
		t.Fatalf("an unconfirmed stop must stay pending for the recovery path: %v", err)
	}
}

// Recovery: an unreadable unit state is unknown, and a stopping unit is
// alive. Neither may turn a missing receipt into an aborted pull or a
// review demand.
func TestCompletedPreparationWorkNeedsAReadableStoppedUnit(t *testing.T) {
	for _, tc := range []struct{ step, systemctl string }{
		{"pull", "exit 1"},
		{"build", "exit 1"},
		{"pull", "echo deactivating"},
		{"build", "echo deactivating"},
	} {
		t.Run(tc.step+" "+tc.systemctl, func(t *testing.T) {
			d, r, bin := workFixture(t, tc.step, "exit 0")
			setSystemctl(t, bin, tc.systemctl)
			if _, err := d.CompletedPreparationWork(r, r.Work.CommandID); !errors.Is(err, ErrPreparationPending) {
				t.Fatalf("%s without a receipt, unit %q: want pending, got %v", tc.step, tc.systemctl, err)
			}
		})
	}
}

// Recovery reads the receipt once more after a stopped reading: a pull that
// finished right before the check is ready, not aborted.
func TestCompletedPreparationWorkRereadsTheReceipt(t *testing.T) {
	d, r, bin := workFixture(t, "pull", "exit 0")
	dir, err := d.workDirectory(r.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(preparationWorkResult{Version: 1, ID: r.Work.ID, RequestSHA256: r.Work.RequestSHA256, Completed: true, Success: true})
	staged := filepath.Join(bin, "receipt.json")
	if err := os.WriteFile(staged, receipt, 0o600); err != nil {
		t.Fatal(err)
	}
	setSystemctl(t, bin, "cp '"+staged+"' '"+filepath.Join(dir, "result.json")+"'; chmod 600 '"+filepath.Join(dir, "result.json")+"'\necho inactive")
	next, err := d.CompletedPreparationWork(r, r.Work.CommandID)
	if err != nil || next == nil || next.Phase != "ready" {
		t.Fatalf("a receipt that landed with the stop was ignored: %+v %v", next, err)
	}
}
