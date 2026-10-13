package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// quiesceDropJob removes the restore job's container from the double: the
// container disappeared after the agent released the exchange, with no exit
// code left behind (an external `docker rm -f`, a daemon that lost it).
func quiesceDropJob(t *testing.T, path string) {
	quiesceSwapState(t, path, func(state *dockertest.State) {
		kept := state.Containers[:0]
		for _, c := range state.Containers {
			if c.Config.Labels["com.docker.compose.project"] != quiesceTestJob {
				kept = append(kept, c)
			}
		}
		state.Containers = kept
	})
}

// quiesceRunVanishingJob releases a quiesce restore whose job, once it sees GO, runs swap
// (when given) and then vanishes. It returns the terminal result.
func quiesceRunVanishingJob(t *testing.T, swap func(d *Docker)) (*Docker, *dockertest.Double, sdkclient.DeployResult) {
	t.Helper()
	quiesceShortDeadlines(t)
	// A ceiling the test never reaches: a vanished job must not be read as
	// an exchange that ran past it.
	original := quiesceExitCeiling
	quiesceExitCeiling = 10 * time.Minute
	t.Cleanup(func() { quiesceExitCeiling = original })
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	writeReady(t, d)
	go func() {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if exists(filepath.Join(quiesceStagingDir(d), "GO")) {
				if swap != nil {
					swap(d)
				}
				for time.Now().Before(deadline) {
					quiesceDropJob(t, statePath)
					time.Sleep(500 * time.Millisecond)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	done := make(chan sdkclient.DeployResult, 1)
	go func() {
		done <- d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_vanish", Kind: sdkclient.CommandDeploy,
			Payload: quiescePayload(quiesceTestTarget, nil)})
	}()
	select {
	case result := <-done:
		return d, double, result
	case <-time.After(150 * time.Second):
		t.Fatal("the vanished job held the command")
		return nil, nil, sdkclient.DeployResult{}
	}
}

// The job container disappears after GO without touching the
// data. The agent used to wait ~30 polls, call it an exchange that "did not
// finish within" the ceiling, fail the undo of a swap that never happened,
// and tell the customer to contact support before restarting an
// application it had just started. Now: the true reason, nothing undone,
// the application back on its own data.
func TestQuiesceVanishedJobWithoutSwapIsReportedAsSuch(t *testing.T) {
	d, double, result := quiesceRunVanishingJob(t, nil)
	if result.Status != "failed" || !strings.Contains(result.Error, "disappeared before reporting a result") {
		t.Fatalf("the vanished job was not reported for what it is: %q", result.Error)
	}
	for _, wrong := range []string{"did not finish within", "contact support", "rollback did not complete"} {
		if strings.Contains(result.Error, wrong) {
			t.Fatalf("the result still says %q: %q", wrong, result.Error)
		}
	}
	if proof := result.RestoreQuiesce; proof == nil || proof.Undone {
		t.Fatalf("a swap that never happened was reported as undone: %+v", proof)
	}
	current, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"))
	if err != nil || string(current) != "restored" {
		t.Fatalf("the application's own data changed: %q %v", current, err)
	}
	if c, ok := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); !ok || c.State.Status != "running" {
		t.Fatalf("the application did not come back: %+v", c)
	}
}

// The job swapped the data and then vanished. The displaced tree says
// the exchange started, so it is undone — and the message says the job
// disappeared, not that a ceiling passed.
func TestQuiesceVanishedJobAfterSwapIsUndone(t *testing.T) {
	d, _, result := quiesceRunVanishingJob(t, func(d *Docker) {
		old := filepath.Join(d.appDir(quiesceTestTarget), "data.replaced-20261012-010000")
		if _, err := os.Stat(old); err == nil {
			return
		}
		_ = os.MkdirAll(old, 0o700)
		_ = os.WriteFile(filepath.Join(old, "current.txt"), []byte("previous"), 0o600)
		_ = os.WriteFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"), []byte("half-restored"), 0o600)
	})
	if result.Status != "failed" || !strings.Contains(result.Error, "disappeared after the data exchange started; the exchange was undone") {
		t.Fatalf("the vanished swap was not reported and undone: %q", result.Error)
	}
	if strings.Contains(result.Error, "did not finish within") {
		t.Fatalf("a vanished job was reported as a stall: %q", result.Error)
	}
	if proof := result.RestoreQuiesce; proof == nil || !proof.Undone {
		t.Fatalf("the swap was not undone: %+v", proof)
	}
	current, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"))
	if err != nil || string(current) != "previous" {
		t.Fatalf("the previous data is not back: %q %v", current, err)
	}
}

// The exchange deadline is pinned in the journal. An agent restart
// resumes the time left — here none — instead of granting a fresh ceiling,
// so a restore never holds the queue for more than the ceilings in total.
func TestQuiesceExchangeDeadlineSurvivesRestart(t *testing.T) {
	quiesceShortDeadlines(t)
	original := quiesceExitCeiling
	quiesceExitCeiling = 10 * time.Minute
	t.Cleanup(func() { quiesceExitCeiling = original })
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	writeReady(t, d)
	plan := quiescePlan{Target: quiesceTestTarget, BackupID: quiesceTestBackup, StopTimeout: 10}
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_quiesce_restart", plan); err != nil {
		t.Fatal(err)
	}
	record, err := d.loadQuiesceRecord(quiesceTestJob)
	if err != nil || record == nil {
		t.Fatal(err)
	}
	// The process that released the exchange pinned its deadline and died;
	// the deadline has passed by the time the agent comes back.
	record.Phase = "stopped"
	record.ExitDeadlineUTC = time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	if err := d.saveQuiesceRecord(*record); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan sdkclient.DeployResult, 1)
	go func() {
		done <- d.quiesceDrive(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_restart"}, quiesceTestJob)
	}()
	select {
	case result := <-done:
		if !strings.Contains(result.Error, "did not finish within") {
			t.Fatalf("the resumed exchange did not end on its pinned deadline: %q", result.Error)
		}
		if elapsed := time.Since(start); elapsed > 60*time.Second {
			t.Fatalf("the resumed exchange took %s: the restart granted a fresh ceiling", elapsed)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the resumed exchange waited a fresh ceiling instead of the pinned deadline")
	}

	// A journal without a deadline (written by an older agent) gets one
	// pinned at the first wait, so the next restart counts from there.
	stamp := ""
	at := quiesceDeadline(&stamp, time.Hour)
	if stamp == "" || time.Until(at) < 59*time.Minute || time.Until(at) > time.Hour {
		t.Fatalf("no deadline was pinned for a journal without one: %q %s", stamp, at)
	}
	again := quiesceDeadline(&stamp, 5*time.Hour)
	if !again.Equal(at) {
		t.Fatalf("a pinned deadline moved: %s then %s", at, again)
	}
	bad := restoreQuiesceRecord{Version: 1, CommandID: "cmd_ok", JobID: quiesceTestJob, BackupID: quiesceTestBackup,
		Target: quiesceTestTarget, StopTimeoutSec: 60, Phase: "stopped", ExitDeadlineUTC: "tomorrow"}
	if err := bad.validate(); err == nil {
		t.Fatal("an unreadable deadline was accepted")
	}
}

// The startup reconciliation of an unowned journal must outlast the
// ceilings the drive itself honours; 45 min against 2 h made the startup
// fail and the agent restart into the same journal again.
func TestQuiesceReconcileBudgetCoversTheCeilings(t *testing.T) {
	if quiesceReconcileBudget() < quiesceReadyCeiling+quiesceExitCeiling+quiesceAbortWait {
		t.Fatalf("the reconcile budget %s is shorter than the ceilings (%s + %s + %s)",
			quiesceReconcileBudget(), quiesceReadyCeiling, quiesceExitCeiling, quiesceAbortWait)
	}
}

// A corrupt journal whose directory holds a result.json written
// for ANOTHER job must not poison the application that receipt names — that
// left the real target, mid-exchange, free. Without a receipt of its own,
// the target is unknown and the poison is global.
func TestQuiescePoisonIgnoresAnotherJobsReceipt(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	const otherJob, otherApp = "bkpjob_fedcba9876543210", "dpl_bbbbbbbbbbbbbbbb"
	d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_other_job", Status: "failed",
		DeploymentID: otherJob, RestoreQuiesce: &sdkclient.RestoreQuiesceResult{Target: otherApp}})
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("a corrupt journal took the agent down: %v", err)
	}
	if _, wrong := d.quiescePoisoned[otherApp]; wrong {
		t.Fatalf("another job's receipt poisoned the application it names: %v", d.quiescePoisoned)
	}
	if _, global := d.quiescePoisoned[""]; !global {
		t.Fatalf("without a receipt of its own the poison is not global: %v", d.quiescePoisoned)
	}
	// The recorded result is not handed out for a job it does not belong to.
	if _, ok := d.quiesceReadResult(quiesceTestJob, "cmd_other_job"); ok {
		t.Fatal("another job's result was answered as this journal's")
	}
}
