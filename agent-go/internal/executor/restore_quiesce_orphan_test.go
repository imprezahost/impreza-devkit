package executor

// A journal whose job survived the teardown is terminal. The
// reconcile at agent startup must NOT start the application from it — the
// job is still writing into data/, and starting the app on that tree mixes
// the two generations. The application stays stopped until support acts.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// dataFiles lists the regular files under the target's data/, to prove the
// reconcile itself writes nothing there.
func dataFiles(t *testing.T, d *Docker) string {
	t.Helper()
	var names []string
	_ = filepath.Walk(filepath.Join(d.appDir(quiesceTestTarget), "data"), func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			names = append(names, filepath.Base(path))
		}
		return nil
	})
	return strings.Join(names, ",")
}

// orphanJournal writes a quiesce_orphan journal for the target with the
// given command id, the state the survivor path leaves on disk.
func orphanJournal(t *testing.T, d *Docker, commandID string) string {
	t.Helper()
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := &restoreQuiesceRecord{
		Version: 1, CommandID: commandID, JobID: quiesceTestJob, BackupID: quiesceTestBackup,
		Target: quiesceTestTarget, StopTimeoutSec: 10, Phase: "quiesce_orphan", Outcome: "fail_unchanged",
		JobExitCode: -1,
	}
	if err := d.saveQuiesceRecord(*record); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestQuiesceOrphanJournalDoesNotStartAppOnReconcile proves the fix: the
// reconcile finds an ownerless journal in quiesce_orphan (the survivor path
// wrote it), and the application stays stopped — no container is started,
// no file in data/ changes after the result, and a new restore on the
// quarantined target is refused. On the pre-fix code the same journal said
// "starting" and the reconcile brought the application up on data a live
// job was still writing into.
func TestQuiesceOrphanJournalDoesNotStartAppOnReconcile(t *testing.T) {
	quiesceShortDeadlines(t)
	original := quiesceExitCeiling
	quiesceExitCeiling = 5 * time.Second
	t.Cleanup(func() { quiesceExitCeiling = original })
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	writeReady(t, d)
	// The job container survives compose down.
	state := double.State()
	for i := range state.Containers {
		if state.Containers[i].Config.Labels["com.docker.compose.project"] == quiesceTestJob {
			state.DownSurvivors = append(state.DownSurvivors, state.Containers[i].ID)
		}
	}
	double.Save(state)
	s3done := s3Script(t, d, double.StatePath())

	// Own deadline: a regression that hangs the exchange fails here in
	// two minutes instead of waiting for the suite timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Drive the restore to the survivor terminal state.
	result := d.Execute(ctx, &sdkclient.PollCommand{ID: "cmd_orphan", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)})
	close(s3done) // stop the writer: nothing may touch data/ from here on
	if result.Status != "failed" || !strings.Contains(result.Error, "could not be stopped") {
		t.Fatalf("the survivor path should produce a failed result: %+v", result)
	}
	// The journal is on disk. Which terminal phase it carries is checked
	// after the reconcile; what matters here is only that the restore
	// reached a terminal state without touching data/ again.
	record, err := d.loadQuiesceRecord(quiesceTestJob)
	if err != nil || record == nil {
		t.Fatalf("no journal after the survivor result: %+v %v", record, err)
	}
	t.Logf("journal phase after the survivor result: %s", record.Phase)
	// The app was stopped by the quiesce; nothing started it yet.
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		if c, ok := double.Container(name); !ok || c.State.Status == "running" {
			t.Fatalf("the application should be stopped before the reconcile: %+v", c)
		}
	}
	before := dataFiles(t, d)

	// A new restore on the same application is refused BEFORE any restart:
	// the quarantined journal on disk is enough, with a different job id
	// (the same id is already refused by the per-job journal conflict).
	freshJob := d.Execute(ctx, &sdkclient.PollCommand{ID: "cmd_fresh_job", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, map[string]any{"deployment_id": "bkpjob_fedcba9876543210"})})
	if freshJob.Status != "failed" || !strings.Contains(freshJob.Error, "quarantined") {
		t.Fatalf("a new restore was accepted on a target with a quarantined journal: %+v", freshJob)
	}

	// The journal is ownerless (no active command). The reconcile must
	// NOT start the application from it.
	if err := d.ReconcileRestoreQuiesce(ctx, "cmd_someone_else"); err != nil {
		t.Fatalf("reconcile took the agent down: %v", err)
	}
	// The application stays stopped.
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		if c, ok := double.Container(name); !ok || c.State.Status == "running" {
			t.Fatalf("the reconcile started the application from a quiesce_orphan journal: %+v", c)
		}
	}
	// data/ is unchanged after the result: the reconcile wrote nothing.
	if after := dataFiles(t, d); after != before {
		t.Fatalf("data/ changed during the reconcile: before=%s after=%s", before, after)
	}
	// The journal is still quarantined (not forgotten).
	if kept, err := d.loadQuiesceRecord(quiesceTestJob); err != nil || kept == nil || kept.Phase != "quiesce_orphan" {
		t.Fatalf("the reconcile released the orphan journal: %+v %v", kept, err)
	}
	// The target is poisoned: a new restore on the same app is refused.
	newRestore := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_after_orphan", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)})
	if newRestore.Status != "failed" || !strings.Contains(newRestore.Error, "quarantined") {
		t.Fatalf("a new restore on the quarantined target was not refused: %+v", newRestore)
	}
}

// TestQuiesceOrphanJournalReleasedWhenJobDead proves the other branch: when
// the job has since died, the reconcile releases the journal (the result is
// already recorded) without starting the application. This is also the
// Guard: a mutation that counts an exited container as alive
// makes this test fail — the journal would be kept quarantined with the
// job dead.
func TestQuiesceOrphanJournalReleasedWhenJobDead(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	// The application was quiesced (stopped); the job survived the
	// teardown and has since died on its own — its container is still
	// there, exited, because compose down could not remove it.
	double := quiesceContainers(t, d, "exited", "exited", 137)
	dir := orphanJournal(t, d, "cmd_orphan_dead")
	d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_orphan_dead", Status: "failed",
		DeploymentID:   quiesceTestJob,
		RestoreQuiesce: &sdkclient.RestoreQuiesceResult{Target: quiesceTestTarget, Stopped: true, JobExitCode: -1},
		Error:          "quiesce_orphan: the job could not be stopped and the application remains stopped."})
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	// The journal directory is gone (released), not merely unreadable.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("the reconcile kept the orphan journal after releasing it: %v", statErr)
	}
	// The application was NOT started by the release.
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		if c, ok := double.Container(name); !ok || c.State.Status == "running" {
			t.Fatalf("the release started the application: %+v", c)
		}
	}
}

// A paused or restarting job counts as alive — it can resume
// writing into data/ at any moment. A mutation that drops paused and
// restarting from the alive set releases the journal here and fails this
// test.
func TestQuiesceOrphanPausedJobCountsAsAlive(t *testing.T) {
	for _, status := range []string{"paused", "restarting"} {
		t.Run(status, func(t *testing.T) {
			quiesceShortDeadlines(t)
			d := quiesceFixture(t)
			quiesceContainers(t, d, "exited", status, 0)
			dir := orphanJournal(t, d, "cmd_orphan_paused")
			if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}
			// The journal stays quarantined: the directory must exist.
			if _, statErr := os.Stat(dir); statErr != nil {
				t.Fatalf("the reconcile released a journal whose job is %s", status)
			}
		})
	}
}

// An inspection error counts as alive. With the double's
// state unreadable every docker command fails; the reconcile must keep the
// journal quarantined rather than treat the job as dead. A mutation that
// treats the error as dead releases the journal here and fails this test.
func TestQuiesceOrphanInspectErrorCountsAsAlive(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "exited", "running", 0)
	dir := orphanJournal(t, d, "cmd_orphan_blind")
	// The double cannot read its own state: every inspection errors.
	if err := os.WriteFile(double.StatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	// The journal stays quarantined: the directory must exist.
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatal("the reconcile released a journal it could not inspect")
	}
}

// TestQuiesceUnownedJournalWithLiveJobDoesNotStartApp covers the guard that
// applies to EVERY journal the reconcile drives to completion: a crash can
// leave an ordinary journal (here mid-startup after a successful exchange)
// with the job container still alive. Removing the pre-drive check starts
// the application on data a live job may still be touching — this test is
// what catches that mutation.
func TestQuiesceUnownedJournalWithLiveJobDoesNotStartApp(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "exited", "running", 0)
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := &restoreQuiesceRecord{
		Version: 1, CommandID: "cmd_crashed_starting", JobID: quiesceTestJob, BackupID: quiesceTestBackup,
		Target: quiesceTestTarget, StopTimeoutSec: 10, Phase: "starting", Outcome: "succeed",
		JobExitCode: 0,
	}
	if err := d.saveQuiesceRecord(*record); err != nil {
		t.Fatal(err)
	}
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	// The application was NOT started: the job is alive.
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		if c, ok := double.Container(name); !ok || c.State.Status == "running" {
			t.Fatalf("the reconcile drove an unowned journal with a live job and started the application: %+v", c)
		}
	}
	// The journal is kept for the next attempt (the job may die later).
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatal("the reconcile discarded the unowned journal without driving it")
	}
}
