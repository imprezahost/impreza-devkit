package executor

// Guard tests of restore-quiesce-v1. Each one pins one safety property
// with its own short deadlines, so a broken guard fails in seconds
// instead of leaning on the suite timeout.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// quiesceShortDeadlines gives each guard test its own tight budget: a
// broken guard fails in seconds instead of leaning on the global go test
// timeout.
func quiesceShortDeadlines(t *testing.T) {
	t.Helper()
	ready, abort, exit := quiesceReadyCeiling, quiesceAbortWait, quiesceExitCeiling
	quiesceReadyCeiling, quiesceAbortWait, quiesceExitCeiling = 20*time.Second, 3*time.Second, 30*time.Second
	t.Cleanup(func() { quiesceReadyCeiling, quiesceAbortWait, quiesceExitCeiling = ready, abort, exit })
}

// G3: `docker stop` answering success while the container keeps Running
// must abort the quiesce without touching any data.
func TestQuiesceAbortsWhenTheStopLies(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	writeReady(t, d)
	state := double.State()
	state.LyingStops = []string{state.Containers[0].ID}
	double.Save(state)
	watch := quiesceWatch(func() bool {
		return exists(filepath.Join(quiesceStagingDir(d), "ABORT"))
	}, func() { quiesceExitJob(t, statePath, 1) }, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-watch }()

	cmd := &sdkclient.PollCommand{ID: "cmd_quiesce_g3", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)}
	result := d.Execute(context.Background(), cmd)
	if result.Status != "failed" || !strings.Contains(result.Error, "could not be stopped") {
		t.Fatalf("the lying stop did not abort the restore: %+v", result)
	}
	if proof := result.RestoreQuiesce; proof == nil || proof.Stopped {
		t.Fatalf("the proof must show the stop never completed: %+v", proof)
	}
	if _, err := os.Stat(filepath.Join(quiesceStagingDir(d), "GO")); !os.IsNotExist(err) {
		t.Fatal("the exchange was released after a lying stop")
	}
	if _, err := os.Stat(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt")); err != nil {
		t.Fatal("application data was touched after a lying stop")
	}
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		if c, ok := double.Container(name); !ok || c.State.Status != "running" {
			t.Fatalf("application container not running after the abort: %+v", c)
		}
	}
	if c, _ := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); c.HostConfig.RestartPolicy.Name != "always" {
		t.Fatalf("restart policy not restored after the lying-stop abort: %+v", c.HostConfig)
	}
}

// G5: the undo is inferred from the displaced tree alone — the job exits
// with a generic failure code, not the swapped-failure contract.
func TestQuiesceUndoInferredFromDisplacedTreeWithoutSwappedExitCode(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	plan := quiescePlan{Target: quiesceTestTarget, BackupID: quiesceTestBackup, StopTimeout: 10}
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_quiesce_g5", plan); err != nil {
		t.Fatal(err)
	}
	// Staging is already done when the interrupted command is resumed, and
	// the job's own project exists so its teardown works: the scripted
	// watcher then settles as soon as the agent consumes the outcome,
	// instead of re-asserting through a long window.
	jobDir := d.appDir(quiesceTestJob)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "compose.yaml"), []byte("services:\n  job:\n    image: busybox:1.37\n    restart: \"no\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReady(t, d)
	watch := quiesceWatch(func() bool {
		return exists(filepath.Join(quiesceStagingDir(d), "GO"))
	}, func() {
		old := filepath.Join(d.appDir(quiesceTestTarget), "data.replaced-20260928-180000")
		if _, err := os.Stat(old); err == nil {
			quiesceExitJob(t, statePath, 1)
			return
		}
		if os.MkdirAll(old, 0o700) != nil {
			return
		}
		_ = os.Rename(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"), filepath.Join(old, "current.txt"))
		_ = os.WriteFile(filepath.Join(old, "current.txt"), []byte("previous"), 0o600)
		_ = os.WriteFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "restored.txt"), []byte("restored"), 0o600)
		quiesceExitJob(t, statePath, 1)
	}, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-watch }()

	result, found := d.RecoverRestoreQuiesce(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_g5"})
	if !found || result.Status != "failed" {
		t.Fatalf("unexpected result: found=%v %+v", found, result)
	}
	if proof := result.RestoreQuiesce; proof == nil || !proof.Undone {
		t.Fatalf("the displaced tree alone did not drive the rollback: %+v", proof)
	}
	current, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"))
	if err != nil || string(current) != "previous" {
		t.Fatalf("the previous data is not live after the inferred rollback: %q %v", current, err)
	}

}

// G6 + G7: the application comes back on its existing containers with no
// build and no pull, and the job project is torn down without --rmi.
func TestQuiesceRestartAndTeardownComposeFlags(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	readyDone := quiesceWatch(func() bool { return quiesceComposeUpSeen(statePath) }, func() {
		if !exists(filepath.Join(quiesceStagingDir(d), "READY")) {
			writeReady(t, d)
		}
	}, func() bool { return exists(filepath.Join(quiesceStagingDir(d), "READY")) })
	defer func() { <-readyDone }()
	swapDone := quiesceWatch(func() bool {
		return exists(filepath.Join(quiesceStagingDir(d), "GO"))
	}, func() {
		old := filepath.Join(d.appDir(quiesceTestTarget), "data.replaced-20260928-190000")
		if _, err := os.Stat(old); err == nil {
			quiesceExitJob(t, statePath, 0)
			return
		}
		if os.MkdirAll(old, 0o700) != nil {
			return
		}
		_ = os.Rename(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"), filepath.Join(old, "current.txt"))
		_ = os.WriteFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "restored.txt"), []byte("restored"), 0o600)
		quiesceExitJob(t, statePath, 0)
	}, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-swapDone }()

	result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_g67", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)})
	if result.Status != "success" {
		t.Fatalf("restore did not succeed: %s", result.Error)
	}
	targetDir := d.appDir(quiesceTestTarget)
	jobDir := d.appDir(quiesceTestJob)
	var sawTargetUp, sawDown bool
	for _, call := range double.State().Calls {
		joined := strings.Join(call.Args, " ")
		if call.Args[0] == "compose" && call.Dir == targetDir && strings.Contains(joined, " up ") {
			sawTargetUp = true
			if !strings.Contains(joined, "--no-build") || !strings.Contains(joined, "--pull never") {
				t.Fatalf("the application restart would build or pull: %s", joined)
			}
		}
		if call.Args[0] == "compose" && call.Dir == jobDir && strings.Contains(joined, " down") {
			sawDown = true
			if strings.Contains(joined, "--rmi") {
				t.Fatalf("the job teardown removes images (every job would re-download): %s", joined)
			}
		}
	}
	if !sawTargetUp || !sawDown {
		t.Fatalf("expected calls missing (target up=%v, job down=%v)", sawTargetUp, sawDown)
	}
}

// G8: the journal is created with the private modes the fence journals
// use. Windows cannot express the distinction, so the guard asserts where
// it runs for real (Linux, including a disposable test server).
func TestQuiesceJournalPermissions(t *testing.T) {
	if quiesceJournalDirMode != 0o700 || quiesceJournalFileMode != 0o600 {
		t.Fatalf("journal modes drifted from the fence policy: dir %o file %o", quiesceJournalDirMode, quiesceJournalFileMode)
	}
	if runtime.GOOS == "windows" {
		t.Skip("directory/file mode bits are not distinguishable on disk on this station; the mode policy is asserted above")
	}
	d := quiesceFixture(t)
	plan := quiescePlan{Target: quiesceTestTarget, BackupID: quiesceTestBackup, StopTimeout: 10}
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_quiesce_g8", plan); err != nil {
		t.Fatal(err)
	}
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("journal directory mode: %v %v", info.Mode().Perm(), err)
	}
	info, err = os.Stat(filepath.Join(dir, "record.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", info.Mode().Perm(), err)
	}
	d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_quiesce_g8", Status: "success"})
	info, err = os.Stat(filepath.Join(dir, "result.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("result mode: %v %v", info.Mode().Perm(), err)
	}
}

// A corrupt journal fails closed for the application its recorded receipt
// names — and only for it. The target comes from the result file alone: a
// stale staging directory of another application must never redirect the
// poison to that other app, leaving the real one free.
func TestCorruptJournalFailsClosedPerDeploymentNotPerAgent(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	// The corrupt journal, with its terminal result naming the target —
	// and a stale staging directory inside ANOTHER application, which is
	// exactly what used to poison the wrong one.
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := "dpl_bbbbbbbbbbbbbbbb"
	if err := os.MkdirAll(filepath.Join(d.appDir(other), ".restore-bkp_ffffffffffffffff"), 0o700); err != nil {
		t.Fatal(err)
	}
	d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_quiesce_poison", Status: "failed",
		DeploymentID: quiesceTestJob, RestoreQuiesce: &sdkclient.RestoreQuiesceResult{Target: quiesceTestTarget}})
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("a corrupt journal took the whole agent down: %v", err)
	}
	if _, poisoned := d.quiescePoisoned[other]; poisoned {
		t.Fatalf("the stale staging directory poisoned the wrong application: %v", d.quiescePoisoned)
	}

	cmd := &sdkclient.PollCommand{ID: "cmd_quiesce_poison", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)}
	result := d.Execute(context.Background(), cmd)
	if result.Status != "failed" || !strings.Contains(result.Error, "cannot be verified") ||
		!strings.Contains(result.Error, "manual reconciliation") {
		t.Fatalf("the poisoned application's deploy was not refused with the reason: %+v", result)
	}
	// Another application's command still runs (it fails for its own
	// reasons, never for the poison).
	otherCmd := &sdkclient.PollCommand{ID: "cmd_other", Kind: sdkclient.CommandRestart,
		Payload: mustJSON(t, map[string]any{"deployment_id": other})}
	otherResult := d.Execute(context.Background(), otherCmd)
	if strings.Contains(otherResult.Error, "cannot be verified") {
		t.Fatalf("the poison leaked to another application: %+v", otherResult)
	}
	// The refusal reached the control plane with the condition in it, and
	// no container was touched: the guard runs before anything else.
	if result.RestoreQuiesce != nil {
		t.Fatalf("the refusal is not a quiesce receipt: %+v", result.RestoreQuiesce)
	}
}

// A corrupt journal that names no application refuses the data-touching
// operations of EVERY deployment until support reconciles it: with the
// target unreadable, any of them could be the one mid-exchange.
func TestCorruptJournalWithoutTargetRefusesAllDataOperations(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Staging directories under two applications: ambiguity, and no
	// readable receipt to break it.
	for _, app := range []string{quiesceTestTarget, "dpl_bbbbbbbbbbbbbbbb"} {
		if err := os.MkdirAll(filepath.Join(d.appDir(app), ".restore-"+quiesceTestBackup), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("a corrupt journal took the whole agent down: %v", err)
	}
	if _, global := d.quiescePoisoned[""]; !global {
		t.Fatalf("an unreadable target did not fail closed globally: %v", d.quiescePoisoned)
	}
	for _, app := range []string{quiesceTestTarget, "dpl_bbbbbbbbbbbbbbbb"} {
		result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_m6_" + app,
			Kind: sdkclient.CommandRestart, Payload: mustJSON(t, map[string]any{"deployment_id": app})})
		if result.Status != "failed" || !strings.Contains(result.Error, "names no application") {
			t.Fatalf("a data operation on %s was not refused globally: %+v", app, result)
		}
	}
	// A read-only command still runs: the refusal is scoped to operations
	// that change data.
	health := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_m6_health",
		Kind: sdkclient.CommandHealthCheck, Payload: mustJSON(t, map[string]any{"deployment_id": quiesceTestTarget})})
	if strings.Contains(health.Error, "names no application") {
		t.Fatalf("the global refusal leaked to a read-only command: %+v", health)
	}
}

// The wait for the job to finish the data exchange is bounded: past the
// ceiling the exchange is rolled back and the refusal says why, instead of
// holding the agent's queue forever.
func TestQuiesceExchangeCeilingRollsBackInsteadOfHanging(t *testing.T) {
	quiesceShortDeadlines(t)
	original := quiesceExitCeiling
	quiesceExitCeiling = 6 * time.Second
	t.Cleanup(func() { quiesceExitCeiling = original })
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	writeReady(t, d)
	// The scripted job takes the GO and never exits: the exchange hangs.
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if !exists(filepath.Join(quiesceStagingDir(d), "GO")) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			old := filepath.Join(d.appDir(quiesceTestTarget), "data.replaced-20260929-210000")
			if _, err := os.Stat(old); err != nil {
				_ = os.MkdirAll(old, 0o700)
				_ = os.Rename(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"), filepath.Join(old, "current.txt"))
				_ = os.WriteFile(filepath.Join(old, "current.txt"), []byte("previous"), 0o600)
				_ = os.WriteFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "restored.txt"), []byte("restored"), 0o600)
			}
			// Never exit: the container keeps Running until the teardown.
			time.Sleep(time.Second)
		}
	}()

	done := make(chan sdkclient.DeployResult, 1)
	go func() {
		done <- d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_ceiling", Kind: sdkclient.CommandDeploy,
			Payload: quiescePayload(quiesceTestTarget, nil)})
	}()
	select {
	case result := <-done:
		if result.Status != "failed" || !strings.Contains(result.Error, "did not finish within") {
			t.Fatalf("the stalled exchange was not refused with the ceiling reason: %+v", result)
		}
		if proof := result.RestoreQuiesce; proof == nil || !proof.Undone {
			t.Fatalf("the stalled exchange was not rolled back: %+v", proof)
		}
		current, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"))
		if err != nil || string(current) != "previous" {
			t.Fatalf("the previous data is not live after the ceiling rollback: %q %v", current, err)
		}
		if c, ok := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); !ok || c.State.Status != "running" {
			t.Fatalf("the application did not come back after the ceiling rollback: %+v", c)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the stalled exchange held the command past its own deadline")
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
