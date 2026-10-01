package executor

// The undo of a stalled exchange runs only with the job
// project confirmed dead. A live job keeps writing into data/ while the
// rollback moves trees, mixing the two generations; a job that survives
// the teardown means nothing can be safely undone.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// s3Script starts a job that takes the GO, keeps writing into data/ every
// half-second, and never exits on its own — the agent's ceiling has to
// fire. The writer stops when the job container is gone from the double's
// state (the teardown done before the undo) or when the done
// channel closes.
func s3Script(t *testing.T, d *Docker, statePath string) (done chan struct{}) {
	t.Helper()
	done = make(chan struct{})
	go func() {
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-done:
				return
			default:
			}
			// The job container was removed: the teardown ran, the
			// writer is dead. This is what a real job container does
			// when compose down removes it.
			if raw, err := os.ReadFile(statePath); err == nil {
				if !strings.Contains(string(raw), `"com.docker.compose.project":"`+quiesceTestJob+`"`) {
					return
				}
			}
			if exists(filepath.Join(quiesceStagingDir(d), "GO")) {
				old := filepath.Join(d.appDir(quiesceTestTarget), "data.replaced-20260929-230000")
				if _, err := os.Stat(old); err != nil {
					_ = os.MkdirAll(old, 0o700)
					_ = os.WriteFile(filepath.Join(old, "previous.txt"), []byte("previous"), 0o600)
					_ = os.WriteFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "restored.txt"), []byte("restored"), 0o600)
				}
				// Keep writing: a live job pollutes data/ during any
				// premature rollback.
				_ = os.WriteFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "job-write.txt"), []byte("job-was-here"), 0o600)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return done
}

// quiesceBoundedCtx gives a stalled-exchange test its own deadline: a regression
// that hangs the exchange fails the test in two minutes instead of waiting
// for the suite timeout.
func quiesceBoundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// S3a: the job keeps writing until the ceiling fires. The agent tears the
// job down, confirms nothing is running, and only then rolls back. The
// result says "rolled back" and data/ has only the previous generation —
// no job-written files.
func TestQuiesceStalledExchangeJobIsKilledBeforeRollback(t *testing.T) {
	quiesceShortDeadlines(t)
	original := quiesceExitCeiling
	quiesceExitCeiling = 5 * time.Second
	t.Cleanup(func() { quiesceExitCeiling = original })
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	writeReady(t, d)
	done := s3Script(t, d, double.StatePath())

	result := d.Execute(quiesceBoundedCtx(t), &sdkclient.PollCommand{ID: "cmd_s3a", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)})
	close(done) // stop the writer before inspecting data/
	if result.Status != "failed" {
		t.Fatalf("the stalled exchange should fail: %+v", result)
	}
	if !strings.Contains(result.Error, "rolled back") {
		t.Fatalf("the refusal should say the exchange was rolled back: %s", result.Error[:min(200, len(result.Error))])
	}
	if proof := result.RestoreQuiesce; proof == nil || !proof.Undone {
		t.Fatalf("the receipt should show the rollback: %+v", proof)
	}
	// The previous generation is live and the job's files are gone: the
	// rollback ran with the job dead, not alongside a live writer.
	if _, err := os.Stat(filepath.Join(d.appDir(quiesceTestTarget), "data", "job-write.txt")); !os.IsNotExist(err) {
		t.Fatal("the job was still writing when the rollback ran (mixed generations)")
	}
	if got, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "previous.txt")); err != nil || string(got) != "previous" {
		t.Fatalf("the previous data is not live after the rollback: %q %v", got, err)
	}
	// The job project is gone.
	if _, ok := double.Container("impreza_backup_" + quiesceTestBackup); ok {
		t.Fatal("the job container survived the teardown")
	}
}

// S3b: the job survives the teardown (compose down cannot remove it).
// Nothing is undone, the application stays stopped, and the refusal says
// to contact support — it must NOT claim the data was rolled back.
func TestQuiesceStalledExchangeJobSurvivorRefusesRollback(t *testing.T) {
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
	defer close(s3done)

	result := d.Execute(quiesceBoundedCtx(t), &sdkclient.PollCommand{ID: "cmd_s3b", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, nil)})
	if result.Status != "failed" {
		t.Fatalf("the stalled exchange should fail: %+v", result)
	}
	if !strings.Contains(result.Error, "could not be stopped") || !strings.Contains(result.Error, "Contact support") {
		t.Fatalf("the refusal must say the job could not be stopped and ask for support: %s", result.Error[:min(200, len(result.Error))])
	}
	if strings.Contains(result.Error, "rolled back") || strings.Contains(result.Error, "back in place") {
		t.Fatalf("the refusal must not claim a rollback that did not happen: %s", result.Error[:min(200, len(result.Error))])
	}
	if proof := result.RestoreQuiesce; proof == nil || proof.Undone {
		t.Fatalf("the receipt must not claim an undo: %+v", proof)
	}
	// The application was NOT restarted.
	if c, ok := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); !ok || c.State.Status == "running" {
		t.Fatalf("the application was started on data a live job is writing into: %+v", c)
	}
}
