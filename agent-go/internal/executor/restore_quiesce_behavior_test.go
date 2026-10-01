package executor

// Base-compatible behavior tests: they compile and run against the
// 0.6.23 source (only the Docker double is injected alongside) and must
// FAIL there — an old agent ignores the quiesce block, deploys the job as
// a plain one-shot and reports success while the application was never
// stopped around the exchange. On the quiesce-capable agent they must
// pass. This file deliberately uses no quiesce-only API: raw JSON
// payloads, the executor's public entry point and the double.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const (
	behaviorTarget = "dpl_aaaaaaaaaaaaaaaa"
	behaviorJob    = "bkpjob_0123456789abcdef"
	behaviorBackup = "bkp_0123456789abcdef"
)

// behaviorFixture stands up a target application directory with live data.
func behaviorFixture(t *testing.T) *Docker {
	t.Helper()
	root := t.TempDir()
	d := NewDocker(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	appDir := d.appDir(behaviorTarget)
	if err := os.MkdirAll(filepath.Join(appDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  app:\n    image: app:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "data", "current.txt"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// behaviorPayload builds the deploy of a restore transport job whose script
// speaks the quiesce handshake; dropQuiesce produces the plain job the
// control plane sends to agents without the capability.
func behaviorPayload(t *testing.T, d *Docker, dropQuiesce bool) json.RawMessage {
	t.Helper()
	payload := map[string]any{
		"deployment_id":      behaviorJob,
		"fence_dependencies": []string{behaviorTarget},
		"manifest": map[string]any{
			"name":    "impreza-restore",
			"version": "1",
			"runtime": map[string]any{"type": "docker-compose", "compose_yaml": "services:\n  job:\n    image: busybox:1.37\n    restart: \"no\"\n"},
		},
		"vars": map[string]any{
			"IMPREZA_API":       "https://api.imprezahost.com",
			"IMPREZA_JOB":       behaviorBackup,
			"IMPREZA_JOB_TOKEN": "tokenfixture",
			"IMPREZA_TARGET":    behaviorTarget,
			"IMPREZA_CHUNK_MB":  "256",
			"IMPREZA_REPLACED":  "",
			"IMPREZA_LAYOUT":    "2",
			"IMPREZA_ONION":     "skip",
		},
		"routes": []any{},
	}
	if !dropQuiesce {
		payload["quiesce"] = map[string]any{"target": behaviorTarget, "stop_timeout_seconds": 10}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func behaviorContainers(t *testing.T, jobStatus string, jobExit int) *dockertest.Double {
	t.Helper()
	app := dockertest.ComposeContainer(behaviorTarget, "app", "running", 0)
	app.HostConfig.RestartPolicy.Name = "always"
	job := dockertest.ComposeContainer(behaviorJob, "job", jobStatus, jobExit)
	job.Name = "/impreza_backup_" + behaviorBackup
	return dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{app, job}})
}

func behaviorStaging(d *Docker) string {
	return filepath.Join(d.appDir(behaviorTarget), ".restore-"+behaviorBackup)
}

// behaviorScript acts as the quiesce-aware restore script: staging raises READY
// once the container is up, and the exchange plus the exit happen only
// after the agent writes GO. The double's state is replaced atomically,
// through the path the double owns (on Unix the state environment
// variable only exists inside the shim, never in the test process).
func behaviorScript(t *testing.T, d *Docker, double *dockertest.Double, exitCode int) {
	t.Helper()
	path := double.StatePath()
	write := func(m func(*dockertest.State)) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var state dockertest.State
		if json.Unmarshal(raw, &state) != nil {
			return
		}
		m(&state)
		out, err := json.Marshal(state)
		if err != nil {
			return
		}
		tmp := path + ".restore"
		if os.WriteFile(tmp, out, 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}
	upSeen := func() bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var state dockertest.State
		if json.Unmarshal(raw, &state) != nil {
			return false
		}
		for _, call := range state.Calls {
			if len(call.Args) > 0 && call.Args[0] == "compose" && strings.Contains(call.Dir, behaviorJob) {
				for _, arg := range call.Args {
					if arg == "up" {
						return true
					}
				}
			}
		}
		return false
	}
	go func() {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) && !upSeen() {
			time.Sleep(100 * time.Millisecond)
		}
		if err := os.MkdirAll(behaviorStaging(d), 0o700); err != nil {
			return
		}
		_ = os.WriteFile(filepath.Join(behaviorStaging(d), "READY"), []byte("ok\n"), 0o600)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(behaviorStaging(d), "GO")); err == nil {
				old := filepath.Join(d.appDir(behaviorTarget), "data.replaced-20260928-170000")
				if err := os.MkdirAll(old, 0o700); err != nil {
					return
				}
				_ = os.Rename(filepath.Join(d.appDir(behaviorTarget), "data", "current.txt"), filepath.Join(old, "current.txt"))
				_ = os.WriteFile(filepath.Join(d.appDir(behaviorTarget), "data", "restored.txt"), []byte("restored"), 0o600)
				// A Docker call the shim saved concurrently can drop a
				// state rewrite, and the agent's exit wait is unbounded:
				// keep the exit asserted until the agent consumed it and
				// tore the job project down.
				for time.Now().Before(deadline) {
					write(func(state *dockertest.State) {
						for i := range state.Containers {
							if state.Containers[i].Config.Labels["com.docker.compose.project"] == behaviorJob {
								state.Containers[i].State.Status = "exited"
								state.Containers[i].State.ExitCode = exitCode
								state.Containers[i].State.Running = false
							}
						}
					})
					time.Sleep(2 * time.Second)
					raw, err := os.ReadFile(path)
					if err != nil {
						return
					}
					var state dockertest.State
					if json.Unmarshal(raw, &state) != nil {
						return
					}
					gone := true
					for i := range state.Containers {
						if state.Containers[i].Config.Labels["com.docker.compose.project"] == behaviorJob {
							gone = false
						}
					}
					if gone {
						return
					}
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

// The regression: a quiesce restore must stop the application around the
// exchange, release it only through the handshake, and report the job's
// real outcome. On 0.6.23 the quiesce block is ignored: no stop, no GO,
// the job never finishes, and the deploy still reports success.
func TestRestoreBehaviorQuiesceRestoreStopsTheAppAroundTheExchange(t *testing.T) {
	d := behaviorFixture(t)
	double := behaviorContainers(t, "running", 0)
	behaviorScript(t, d, double, 0)

	result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_restore_behavior", Kind: sdkclient.CommandDeploy, Payload: behaviorPayload(t, d, false)})
	if result.Status != "success" {
		t.Fatalf("the quiesce restore did not succeed: %s", result.Error)
	}
	// The receipt is asserted on the wire form, so this file compiles
	// against an agent without the quiesce fields too.
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"restore_quiesce"`) {
		t.Fatal("the restore result carries no restore_quiesce receipt: the agent ignored the quiesce block")
	}
	for _, want := range []string{`"stopped":true`, `"target":"` + behaviorTarget + `"`, `"job_exit_code":0`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("the receipt does not prove the verified stop and the job outcome (%s): %s", want, encoded)
		}
	}
	if _, err := os.Stat(filepath.Join(behaviorStaging(d), "GO")); err != nil {
		t.Fatal("the exchange was never released through the handshake")
	}
	if got, err := os.ReadFile(filepath.Join(d.appDir(behaviorTarget), "data", "restored.txt")); err != nil || string(got) != "restored" {
		t.Fatalf("the exchanged data is not live: %q %v", got, err)
	}
	if len(double.Calls("stop")) != 1 {
		t.Fatalf("expected exactly the target's stop, got %d", len(double.Calls("stop")))
	}
	if c, ok := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); !ok || c.State.Status != "running" {
		t.Fatalf("the application did not come back up: %+v", c)
	}
}

// A job that fails after the exchange started must leave the previous data
// live again. On 0.6.23 nothing interprets the failure: the job waits for
// a GO that never comes and the deploy still reports success.
func TestRestoreBehaviorQuiesceRestoreUndoesAFailedExchange(t *testing.T) {
	d := behaviorFixture(t)
	double := behaviorContainers(t, "running", 0)
	behaviorScript(t, d, double, 75)

	result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_restore_undo", Kind: sdkclient.CommandDeploy, Payload: behaviorPayload(t, d, false)})
	if result.Status != "failed" {
		t.Fatalf("a restore that failed after the exchange must not report success")
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"undone":true`) {
		t.Fatalf("the receipt does not prove the rollback: %s", encoded)
	}
	current, err := os.ReadFile(filepath.Join(d.appDir(behaviorTarget), "data", "current.txt"))
	if err != nil || string(current) != "previous" {
		t.Fatalf("the previous data is not live after the rollback: %q %v", current, err)
	}
	if _, err := os.Stat(filepath.Join(d.appDir(behaviorTarget), "data", "restored.txt")); !os.IsNotExist(err) {
		t.Fatal("the rejected generation is still live in the data directory")
	}
}

// The guard: a job without the quiesce block deploys exactly as before on
// both agents — success, nothing stopped, no receipt.
func TestRestoreBehaviorJobWithoutQuiesceRunsAsBefore(t *testing.T) {
	d := behaviorFixture(t)
	double := behaviorContainers(t, "exited", 0)

	result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_restore_plain", Kind: sdkclient.CommandDeploy, Payload: behaviorPayload(t, d, true)})
	if result.Status != "success" {
		t.Fatalf("a plain transport job no longer deploys: %s", result.Error)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), `"restore_quiesce"`) {
		t.Fatal("a job without the quiesce block produced a restore_quiesce receipt")
	}
	if len(double.Calls("stop")) != 0 || len(double.Calls("update")) != 0 {
		t.Fatal("a plain transport job stopped or touched the application")
	}
	if c, ok := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); !ok || c.State.Status != "running" {
		t.Fatalf("the application was disturbed by a plain job: %+v", c)
	}
}
