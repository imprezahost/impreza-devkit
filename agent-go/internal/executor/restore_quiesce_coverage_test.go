package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Coverage gaps of the quiesce restore: mutations that no earlier test
// caught. None of these tests changes behaviour; each one fails under the
// mutation its comment describes.

// The verified stop also checks the restart policy. A daemon that
// acknowledges `docker update --restart=no` without applying it leaves a
// container that a reboot (or the policy itself) brings back while the
// exchange moves its data. The quiesce must refuse to release GO.
func TestQuiesceStopVerificationChecksTheRestartPolicy(t *testing.T) {
	quiesceShortDeadlines(t)
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	writeReady(t, d)
	state := double.State()
	state.LyingUpdates = []string{state.Containers[0].ID}
	double.Save(state)
	watch := quiesceWatch(func() bool {
		return exists(filepath.Join(quiesceStagingDir(d), "ABORT"))
	}, func() { quiesceExitJob(t, statePath, 1) }, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-watch }()

	result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_policy", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)})
	if result.Status != "failed" || !strings.Contains(result.Error, "could not be stopped") {
		t.Fatalf("a restart policy that stayed on did not abort the restore: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(quiesceStagingDir(d), "GO")); !os.IsNotExist(err) {
		t.Fatal("the exchange was released with a restart policy still on")
	}
	if raw, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt")); err != nil || string(raw) != "restored" {
		t.Fatalf("application data was touched: %q %v", raw, err)
	}
}

// A poisoned application is refused by its
// own deployment_id — restart, uninstall and rollback carry no
// IMPREZA_TARGET — while the commands that only read it (health, logs)
// still run.
func TestQuiescePoisonedApplicationRefusedByItsDeploymentID(t *testing.T) {
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
	d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_quiesce_poison", Status: "failed",
		DeploymentID: quiesceTestJob, RestoreQuiesce: &sdkclient.RestoreQuiesceResult{Target: quiesceTestTarget}})
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []sdkclient.CommandKind{sdkclient.CommandRestart, sdkclient.CommandUninstall, sdkclient.CommandRollback} {
		result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_poison_" + string(kind), Kind: kind,
			Payload: mustJSON(t, map[string]any{"deployment_id": quiesceTestTarget})})
		if result.Status != "failed" || !strings.Contains(result.Error, "cannot be verified") {
			t.Errorf("%s of the poisoned application (by its deployment_id) was not refused: %+v", kind, result)
		}
	}
	for _, kind := range []sdkclient.CommandKind{sdkclient.CommandHealthCheck, sdkclient.CommandLogsTail} {
		result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_poison_" + string(kind), Kind: kind,
			Payload: mustJSON(t, map[string]any{"deployment_id": quiesceTestTarget})})
		if strings.Contains(result.Error, "cannot be verified") {
			t.Errorf("%s of the poisoned application was refused, but it touches no data: %+v", kind, result)
		}
	}
}

// An exchange that ran past the ceiling is treated as an exchange that
// moved data even when no displaced tree appeared — the job may have been
// writing in place. The result must not claim that nothing was touched.
func TestQuiesceStalledExchangeWithoutDisplacedTreeIsNotReportedUnchanged(t *testing.T) {
	quiesceShortDeadlines(t)
	original := quiesceExitCeiling
	quiesceExitCeiling = 6 * time.Second
	t.Cleanup(func() { quiesceExitCeiling = original })
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	writeReady(t, d)
	// The job takes GO and never exits, and never parks the old data.
	done := make(chan sdkclient.DeployResult, 1)
	go func() {
		done <- d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_stall", Kind: sdkclient.CommandDeploy,
			Payload: quiescePayload(quiesceTestTarget, nil)})
	}()
	select {
	case result := <-done:
		if result.Status != "failed" || !strings.Contains(result.Error, "did not finish within") {
			t.Fatalf("the stalled exchange was not reported as such: %q", result.Error)
		}
		for _, claim := range []string{"before any data was touched", "without changing any data", "nothing was changed"} {
			if strings.Contains(result.Error, claim) {
				t.Fatalf("a stalled exchange was reported as unchanged (%q): %q", claim, result.Error)
			}
		}
		if !strings.Contains(result.Error, "contact support") {
			t.Fatalf("a stalled exchange with nothing to roll back to does not send the customer to support: %q", result.Error)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the stalled exchange held the command past its own deadline")
	}
}
