package executor

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
	quiesceTestTarget = "dpl_aaaaaaaaaaaaaaaa"
	quiesceTestJob    = "bkpjob_0123456789abcdef"
	quiesceTestBackup = "bkp_0123456789abcdef"
)

func quiesceLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// quiesceFixture stands up the target application directory (compose plus
// live data) and returns the executor rooted at the state directory.
func quiesceFixture(t *testing.T) *Docker {
	t.Helper()
	root := t.TempDir()
	d := NewDocker(root, quiesceLogger())
	appDir := d.appDir(quiesceTestTarget)
	if err := os.MkdirAll(filepath.Join(appDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), []byte("services:\n  app:\n    image: app:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "data", "current.txt"), []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// quiescePayload builds the deploy command of a restore transport job the
// way the control plane sends it, quiesce block included.
func quiescePayload(target string, overrides map[string]any) json.RawMessage {
	vars := map[string]any{
		"IMPREZA_API":       "https://api.imprezahost.com",
		"IMPREZA_JOB":       quiesceTestBackup,
		"IMPREZA_JOB_TOKEN": "tokenfixture",
		"IMPREZA_TARGET":    target,
		"IMPREZA_CHUNK_MB":  "256",
		"IMPREZA_REPLACED":  "",
		"IMPREZA_LAYOUT":    "2",
		"IMPREZA_ONION":     "skip",
	}
	payload := map[string]any{
		"deployment_id": quiesceTestJob,
		"fence_dependencies": []string{func() string {
			if target == "" {
				return quiesceTestTarget
			}
			return target
		}()},
		"manifest": map[string]any{
			"name":    "impreza-restore",
			"version": "1",
			"runtime": map[string]any{
				"type":         "docker-compose",
				"compose_yaml": "services:\n  job:\n    image: curlimages/curl:8.11.1\n    restart: \"no\"\n",
			},
		},
		"vars":   vars,
		"routes": []any{},
		"quiesce": map[string]any{
			"target": target,
		},
	}
	for key, value := range overrides {
		if key == "vars" {
			for name, v := range value.(map[string]any) {
				vars[name] = v
			}
			continue
		}
		if key == "no_quiesce" {
			delete(payload, "quiesce")
			continue
		}
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

// quiesceContainers seeds the double with the target application running
// and the restore job container in the given state.
func quiesceContainers(t *testing.T, d *Docker, targetStatus string, jobStatus string, jobExit int) *dockertest.Double {
	t.Helper()
	web := dockertest.ComposeContainer(quiesceTestTarget, "app", targetStatus, 0)
	web.HostConfig.RestartPolicy.Name = "always"
	worker := dockertest.ComposeContainer(quiesceTestTarget, "worker", targetStatus, 0)
	job := dockertest.ComposeContainer(quiesceTestJob, "job", jobStatus, jobExit)
	job.Name = "/impreza_backup_" + quiesceTestBackup
	state := dockertest.State{Containers: []dockertest.Container{web, worker, job}}
	double := dockertest.Install(t, state)
	return double
}

func quiesceStagingDir(d *Docker) string {
	return filepath.Join(d.appDir(quiesceTestTarget), ".restore-"+quiesceTestBackup)
}

// quiesceSwapState rewrites the double's state atomically through the
// mutation, the way the shim itself persists a Docker action.
func quiesceSwapState(t *testing.T, path string, mutate func(*dockertest.State)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state dockertest.State
	if json.Unmarshal(raw, &state) != nil {
		return
	}
	mutate(&state)
	out, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp := path + ".script"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// quiesceExitJob flips the job's container to an exited state with the
// given code — the script's container finishing after the agent released
// (or refused) the swap.
func quiesceExitJob(t *testing.T, path string, code int) {
	quiesceSwapState(t, path, func(state *dockertest.State) {
		for i := range state.Containers {
			if state.Containers[i].Config.Labels["com.docker.compose.project"] == quiesceTestJob {
				state.Containers[i].State.Status = "exited"
				state.Containers[i].State.ExitCode = code
				state.Containers[i].State.Running = false
			}
		}
	})
}

// quiesceWatch runs the scripted behavior in the background until the
// trigger fires, then keeps re-asserting it until the agent consumed the
// outcome: a Docker call the shim saved concurrently can momentarily drop
// a state rewrite, and the agent's unbounded exit wait must not depend on
// a rewrite that landed in exactly one window.
func quiesceWatch(trigger func() bool, script func(), settled func() bool) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if trigger() {
				for time.Now().Before(deadline) {
					script()
					time.Sleep(2 * time.Second)
					if settled() {
						return
					}
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return done
}

// quiesceJobSettled reports whether the job's container is gone from the
// double's state — the agent has read the outcome by then.
func quiesceJobSettled(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var state dockertest.State
	if json.Unmarshal(raw, &state) != nil {
		return false
	}
	for i := range state.Containers {
		if state.Containers[i].Config.Labels["com.docker.compose.project"] == quiesceTestJob {
			return false
		}
	}
	return true
}

// quiesceComposeUpSeen reads the double's recorded calls for the job's
// `compose up`, the moment the job container is guaranteed to exist.
func quiesceComposeUpSeen(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var state dockertest.State
	if json.Unmarshal(raw, &state) != nil {
		return false
	}
	for _, call := range state.Calls {
		if call.Args[0] != "compose" || !strings.Contains(call.Dir, quiesceTestJob) {
			continue
		}
		// The agent pins the project and file before the subcommand, so
		// "up" can sit anywhere after them.
		for _, arg := range call.Args[1:] {
			if arg == "up" {
				return true
			}
		}
	}
	return false
}

// writeReady marks the job's staging as downloaded and verified.
func writeReady(t *testing.T, d *Docker) {
	t.Helper()
	if err := os.MkdirAll(quiesceStagingDir(d), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quiesceStagingDir(d), "READY"), []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeReplacedTree simulates the script having parked the application's
// previous data under data.replaced-<ts>, including one named volume.
func writeReplacedTree(t *testing.T, d *Docker, stamp string) string {
	t.Helper()
	old := filepath.Join(d.appDir(quiesceTestTarget), "data.replaced-"+stamp)
	if err := os.MkdirAll(filepath.Join(old, "vol", "db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "previous.txt"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "vol", "db", "db.old"), []byte("old-volume"), 0o600); err != nil {
		t.Fatal(err)
	}
	return old
}

func TestQuiescePayloadRefusals(t *testing.T) {
	cases := []struct {
		name     string
		payload  json.RawMessage
		contains string
	}{
		{"wrong target", quiescePayload("dpl_bbbbbbbbbbbbbbbb", map[string]any{"vars": map[string]any{"IMPREZA_TARGET": quiesceTestTarget}}), "does not match the job's IMPREZA_TARGET"},
		{"missing target directory", quiescePayload("dpl_cccccccccccccccc", nil), "not a deployment directory"},
		{"discard job", quiescePayload(quiesceTestTarget, map[string]any{"vars": map[string]any{"IMPREZA_REPLACED": "20260928-120000"}}), "not valid on a discard job"},
		{"timeout out of range", quiescePayload(quiesceTestTarget, map[string]any{"quiesce": map[string]any{"target": quiesceTestTarget, "stop_timeout_seconds": 5}}), "between 10 and 600"},
		{"with routes", quiescePayload(quiesceTestTarget, map[string]any{"routes": []any{map[string]any{"hostname": "app.test.imprezaapps.com", "target_port": 80}}}), "cannot be combined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := quiesceFixture(t)
			double := quiesceContainers(t, d, "running", "running", 0)
			cmd := &sdkclient.PollCommand{ID: "cmd_quiesce_refused", Kind: sdkclient.CommandDeploy, Payload: tc.payload}
			result := d.Execute(context.Background(), cmd)
			if result.Status != "failed" || !strings.Contains(result.Error, "quiesce refused") || !strings.Contains(result.Error, tc.contains) {
				t.Fatalf("unexpected result: %+v", result)
			}
			if calls := double.Calls("stop"); len(calls) != 0 {
				t.Fatal("refused quiesce still touched containers")
			}
			if _, err := os.Stat(filepath.Join(d.StateDir, "operations")); !os.IsNotExist(err) {
				t.Fatal("refused quiesce left a journal behind")
			}
			for _, c := range double.State().Containers {
				if c.State.Status != "running" {
					t.Fatalf("refused quiesce changed a container: %+v", c)
				}
			}
		})
	}
}

func TestQuiesceRestoreHappyPath(t *testing.T) {
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	// The scripted job: staging finishes once the container is up, and
	// the exchange plus the exit happen only after the agent releases
	// the GO — never while the application is still being stopped.
	readyDone := quiesceWatch(func() bool { return quiesceComposeUpSeen(statePath) }, func() {
		if !exists(filepath.Join(quiesceStagingDir(d), "READY")) {
			writeReady(t, d)
		}
	}, func() bool { return exists(filepath.Join(quiesceStagingDir(d), "READY")) })
	defer func() { <-readyDone }()
	swapDone := quiesceWatch(func() bool {
		return exists(filepath.Join(quiesceStagingDir(d), "GO"))
	}, func() {
		// The script's exchange: park the live data, put the restored
		// generation in place, then exit zero.
		old := filepath.Join(d.appDir(quiesceTestTarget), "data.replaced-20260928-130000")
		if _, err := os.Stat(old); err == nil {
			quiesceExitJob(t, statePath, 0)
			return
		}
		if err := os.MkdirAll(old, 0o700); err != nil {
			return
		}
		_ = os.Rename(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt"), filepath.Join(old, "current.txt"))
		_ = os.WriteFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "restored.txt"), []byte("restored"), 0o600)
		quiesceExitJob(t, statePath, 0)
	}, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-swapDone }()

	cmd := &sdkclient.PollCommand{ID: "cmd_quiesce_ok", Kind: sdkclient.CommandDeploy, Payload: quiescePayload(quiesceTestTarget, nil)}
	result := d.Execute(context.Background(), cmd)
	if result.Status != "success" {
		t.Fatalf("restore did not succeed: %+v", result)
	}
	proof := result.RestoreQuiesce
	if proof == nil || proof.Target != quiesceTestTarget || !proof.Stopped || proof.JobExitCode != 0 || proof.Undone {
		t.Fatalf("unexpected proof: %+v", proof)
	}
	// The stop really happened, with the durable restart disable first.
	stops := double.Calls("stop")
	if len(stops) != 2 {
		t.Fatalf("expected one stop per target container, got %d", len(stops))
	}
	for _, call := range stops {
		if !strings.Contains(strings.Join(call.Args, " "), "--time") {
			t.Fatalf("stop without a time budget: %v", call.Args)
		}
	}
	if len(double.Calls("update")) < 3 {
		t.Fatal("restart policies were not disabled and returned")
	}
	// The swap was released and the application came back with its policy.
	if _, err := os.Stat(filepath.Join(quiesceStagingDir(d), "GO")); err != nil {
		t.Fatal("GO marker was not written")
	}
	if _, err := os.Stat(filepath.Join(d.appDir(quiesceTestTarget), "data", "restored.txt")); err != nil {
		t.Fatal("the released swap did not run to completion")
	}
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		c, ok := double.Container(name)
		if !ok || c.State.Status != "running" {
			t.Fatalf("application container not running after the restore: %+v", c)
		}
	}
	if c, _ := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); c.HostConfig.RestartPolicy.Name != "always" {
		t.Fatalf("restart policy not returned: %+v", c.HostConfig)
	}
	// The job project is gone and its outcome is journaled for the ack.
	if _, ok := double.Container("impreza_backup_" + quiesceTestBackup); ok {
		t.Fatal("job container survived the teardown")
	}
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		t.Fatal("terminal result was not journaled")
	}
	record, err := d.loadQuiesceRecord(quiesceTestJob)
	if err != nil || record == nil || record.Phase != "starting" || record.Outcome != "succeed" {
		t.Fatalf("journal not left for the acknowledged result: %+v %v", record, err)
	}
}

func TestQuiesceStagingFailureLeavesAppRunning(t *testing.T) {
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	statePath := double.StatePath()
	// The scripted job dies during staging: once its container is up it
	// runs briefly and exits without ever raising READY.
	watch := quiesceWatch(func() bool { return quiesceComposeUpSeen(statePath) }, func() {
		time.Sleep(300 * time.Millisecond)
		quiesceExitJob(t, statePath, 1)
	}, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-watch }()

	cmd := &sdkclient.PollCommand{ID: "cmd_quiesce_staging", Kind: sdkclient.CommandDeploy, Payload: quiescePayload(quiesceTestTarget, nil)}
	result := d.Execute(context.Background(), cmd)
	if result.Status != "failed" || !strings.Contains(result.Error, "job exit 1") || !strings.Contains(result.Error, "nothing was changed") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.RestoreQuiesce == nil || result.RestoreQuiesce.Stopped {
		t.Fatalf("proof must show the app was never stopped: %+v", result.RestoreQuiesce)
	}
	if calls := double.Calls("stop"); len(calls) != 0 {
		t.Fatal("a staging failure stopped the application")
	}
	if calls := double.Calls("update"); len(calls) != 0 {
		t.Fatal("a staging failure touched restart policies")
	}
	if _, err := os.Stat(filepath.Join(quiesceStagingDir(d), "GO")); !os.IsNotExist(err) {
		t.Fatal("GO was released for a failed staging")
	}
	for _, c := range double.State().Containers {
		if strings.HasPrefix(c.Name, "/"+quiesceTestTarget) && c.State.Status != "running" {
			t.Fatalf("application container not running: %+v", c)
		}
	}
}

func TestQuiesceAbortWhenTargetRefusesStop(t *testing.T) {
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "running", 0)
	writeReady(t, d)
	state := double.State()
	refuser := state.Containers[0].ID
	state.Unstoppable = []string{refuser}
	double.Save(state)
	// The scripted job stands down when the agent refuses to proceed.
	statePath := double.StatePath()
	watch := quiesceWatch(func() bool {
		return exists(filepath.Join(quiesceStagingDir(d), "ABORT"))
	}, func() {
		quiesceExitJob(t, statePath, 1)
	}, func() bool { return quiesceJobSettled(statePath) })
	defer func() { <-watch }()

	cmd := &sdkclient.PollCommand{ID: "cmd_quiesce_abort", Kind: sdkclient.CommandDeploy,
		Payload: quiescePayload(quiesceTestTarget, map[string]any{"quiesce": map[string]any{"target": quiesceTestTarget, "stop_timeout_seconds": 10}})}
	result := d.Execute(context.Background(), cmd)
	if result.Status != "failed" || !strings.Contains(result.Error, "could not be stopped") || !strings.Contains(result.Error, "nothing was changed") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if proof := result.RestoreQuiesce; proof == nil || proof.Stopped || proof.Undone {
		t.Fatalf("unexpected proof: %+v", proof)
	}
	if _, err := os.Stat(filepath.Join(quiesceStagingDir(d), "ABORT")); err != nil {
		t.Fatal("ABORT was not signalled to the job")
	}
	if _, err := os.Stat(filepath.Join(quiesceStagingDir(d), "GO")); !os.IsNotExist(err) {
		t.Fatal("the swap was released after a failed stop")
	}
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		if c, ok := double.Container(name); !ok || c.State.Status != "running" {
			t.Fatalf("application container not left running: %+v", c)
		}
	}
	if c, _ := double.Container("dpl_aaaaaaaaaaaaaaaa-app-1"); c.HostConfig.RestartPolicy.Name != "always" {
		t.Fatalf("restart policy not returned after the abort: %+v", c.HostConfig)
	}
	// Nothing moved: no displaced tree was created or interpreted.
	if _, err := os.Stat(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt")); err != nil {
		t.Fatal("application data was touched on the abort path")
	}
}

func TestQuiesceUndoAfterSwappedFailure(t *testing.T) {
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "exited", quiesceExitSwapped)
	writeReady(t, d)
	// The journal is begun before the swap could exist, the way the live
	// deploy does; the displaced tree appears only after the release, so
	// the baseline in the journal does not contain it.
	plan := quiescePlan{Target: quiesceTestTarget, BackupID: quiesceTestBackup, StopTimeout: 10}
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_quiesce_undo", plan); err != nil {
		t.Fatal(err)
	}
	old := writeReplacedTree(t, d, "20260928-140000")
	// The job swapped the named volume too: the volume's live data is the
	// rejected generation, the previous one waits in the parked tree.
	volumeRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(volumeRoot, "db.new"), []byte("restored-volume"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := double.State()
	state.Volumes = []dockertest.Volume{{Name: quiesceTestTarget + "_db", Mountpoint: volumeRoot}}
	double.Save(state)

	result, found := d.RecoverRestoreQuiesce(context.Background(), &sdkclient.PollCommand{ID: "cmd_quiesce_undo"})
	if !found || result.Status != "failed" || !strings.Contains(result.Error, "undone") {
		t.Fatalf("unexpected result: found=%v %+v", found, result)
	}
	if proof := result.RestoreQuiesce; proof == nil || !proof.Undone || proof.JobExitCode != quiesceExitSwapped {
		t.Fatalf("unexpected proof: %+v", proof)
	}
	current, err := os.ReadFile(filepath.Join(d.appDir(quiesceTestTarget), "data", "previous.txt"))
	if err != nil || string(current) != "previous" {
		t.Fatalf("previous data was not restored: %q %v", current, err)
	}
	if _, err := os.Stat(filepath.Join(d.appDir(quiesceTestTarget), "data", "current.txt")); !os.IsNotExist(err) {
		t.Fatal("the rejected generation is still live in the data directory")
	}
	live, err := os.ReadFile(filepath.Join(volumeRoot, "db.old"))
	if err != nil || string(live) != "old-volume" {
		t.Fatalf("previous volume data was not restored: %q %v", live, err)
	}
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	parked, err := os.ReadFile(filepath.Join(dir, "undo", "data", "current.txt"))
	if err != nil || string(parked) != "restored" {
		t.Fatalf("rejected generation not parked for review: %q %v", parked, err)
	}
	if parked, err := os.ReadFile(filepath.Join(dir, "undo", "vol", "db", "db.new")); err != nil || string(parked) != "restored-volume" {
		t.Fatalf("rejected volume generation not parked: %q %v", parked, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("the displaced tree disappeared before the ack")
	}
	for _, name := range []string{"dpl_aaaaaaaaaaaaaaaa-app-1", "dpl_aaaaaaaaaaaaaaaa-worker-1"} {
		if c, ok := double.Container(name); !ok || c.State.Status != "running" {
			t.Fatalf("application container not running after the undo: %+v", c)
		}
	}
}

func TestQuiesceUndoResumesTwoPass(t *testing.T) {
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "exited", 0)
	writeReplacedTree(t, d, "20260928-150000")
	// The named volume the parked tree holds a previous generation of.
	volumeRoot := t.TempDir()
	state := double.State()
	state.Volumes = []dockertest.Volume{{Name: quiesceTestTarget + "_db", Mountpoint: volumeRoot}}
	double.Save(state)
	// The state an interrupted undo leaves behind: the live data directory
	// was evacuated (pass 1 done), the previous contents never returned.
	appData := filepath.Join(d.appDir(quiesceTestTarget), "data")
	if err := os.RemoveAll(appData); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appData, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "undo", "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "undo", "pass1-data.done"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "undo", "data", "current.txt"), []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := d.quiesceUndoSwap(quiesceTestJob, quiesceTestTarget, "data.replaced-20260928-150000"); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(appData, "previous.txt"))
	if err != nil || string(current) != "previous" {
		t.Fatalf("resumed undo did not return the previous data: %q %v", current, err)
	}
	if _, err := os.Stat(filepath.Join(appData, "current.txt")); !os.IsNotExist(err) {
		t.Fatal("resumed undo mixed the generations")
	}
	live, err := os.ReadFile(filepath.Join(volumeRoot, "db.old"))
	if err != nil || string(live) != "old-volume" {
		t.Fatalf("resumed undo did not return the previous volume: %q %v", live, err)
	}
}

func TestQuiesceRecoverFromJournal(t *testing.T) {
	d := quiesceFixture(t)
	double := quiesceContainers(t, d, "running", "exited", 0)
	writeReady(t, d)
	writeReplacedTree(t, d, "20260928-160000")
	plan := quiescePlan{Target: quiesceTestTarget, BackupID: quiesceTestBackup, StopTimeout: 10}
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_quiesce_resume", plan); err != nil {
		t.Fatal(err)
	}

	cmd := &sdkclient.PollCommand{ID: "cmd_quiesce_resume"}
	result, found := d.RecoverRestoreQuiesce(context.Background(), cmd)
	if !found || result.Status != "success" {
		t.Fatalf("recovery did not finish the restore: found=%v %+v", found, result)
	}
	if result.RestoreQuiesce == nil || !result.RestoreQuiesce.Stopped {
		t.Fatalf("recovery lost the stop proof: %+v", result.RestoreQuiesce)
	}
	callsBefore := len(double.State().Calls)
	again, found := d.RecoverRestoreQuiesce(context.Background(), cmd)
	if !found || again.Status != "success" || again.CommandID != result.CommandID {
		t.Fatalf("recorded outcome did not answer the resend: found=%v %+v", found, again)
	}
	if len(double.State().Calls) != callsBefore {
		t.Fatal("answering from the journal repeated Docker work")
	}
}

func TestQuiesceRecoverPendingWhenContextDies(t *testing.T) {
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "running", 0)
	writeReady(t, d)
	plan := quiescePlan{Target: quiesceTestTarget, BackupID: quiesceTestBackup, StopTimeout: 10}
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_quiesce_dead", plan); err != nil {
		t.Fatal(err)
	}
	// Advance the journal to a stopped release, then kill the context the
	// way an agent shutdown would.
	record, err := d.loadQuiesceRecord(quiesceTestJob)
	if err != nil || record == nil {
		t.Fatal(err)
	}
	record.Phase = "stopped"
	record.Containers = []restoreQuiesceContainerPolicy{{ID: strings.Repeat("a", 64), Restart: "always"}}
	if err := d.saveQuiesceRecord(*record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, found := d.RecoverRestoreQuiesce(ctx, &sdkclient.PollCommand{ID: "cmd_quiesce_dead"})
	if !found || result.Status != PreparationPendingStatus {
		t.Fatalf("dead context should surface as pending, not a terminal result: found=%v %+v", found, result)
	}
	kept, err := d.loadQuiesceRecord(quiesceTestJob)
	if err != nil || kept == nil || kept.Phase != "stopped" {
		t.Fatalf("journal not kept for the next boot: %+v %v", kept, err)
	}
}

func TestQuiesceReconcileOrphans(t *testing.T) {
	d := quiesceFixture(t)
	quiesceContainers(t, d, "running", "exited", 0)

	// An orphaned journal that already recorded its outcome is released.
	plan := quiescePlan{Target: quiesceTestTarget, BackupID: quiesceTestBackup, StopTimeout: 10}
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_quiesce_done", plan); err != nil {
		t.Fatal(err)
	}
	d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_quiesce_done", Status: "success"})
	if err := d.ReconcileRestoreQuiesce(context.Background(), "cmd_active_other"); err != nil {
		t.Fatal(err)
	}
	dir, err := d.quiesceDir(quiesceTestJob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("finished orphan journal was not released")
	}

	// The journal the active command owns is left to the poller.
	if err := d.beginQuiesceRecord(quiesceTestJob, "cmd_active_mine", plan); err != nil {
		t.Fatal(err)
	}
	if err := d.ReconcileRestoreQuiesce(context.Background(), "cmd_active_mine"); err != nil {
		t.Fatal(err)
	}
	if record, err := d.loadQuiesceRecord(quiesceTestJob); err != nil || record == nil {
		t.Fatalf("active command journal was reconciled away: %+v %v", record, err)
	}
	if err := d.forgetQuiesceRecord(quiesceTestJob); err != nil {
		t.Fatal(err)
	}

	// A corrupt journal no longer takes the agent down: the reconciliation
	// succeeds and the deployment its receipt names is refused with the
	// reason (the per-deployment fail-closed).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The terminal result names the target; without it the poison goes
	// global, and with it the right deployment is the one refused.
	d.quiesceWriteResult(quiesceTestJob, sdkclient.DeployResult{CommandID: "cmd_active_mine", Status: "failed",
		DeploymentID: quiesceTestJob, RestoreQuiesce: &sdkclient.RestoreQuiesceResult{Target: quiesceTestTarget}})
	if err := d.ReconcileRestoreQuiesce(context.Background(), ""); err != nil {
		t.Fatalf("a corrupt journal took the whole agent down: %v", err)
	}
	if _, poisoned := d.quiescePoisoned[quiesceTestTarget]; !poisoned {
		t.Fatalf("the corrupt journal did not poison its deployment: %v", d.quiescePoisoned)
	}
}

func TestQuiesceRecordValidation(t *testing.T) {
	valid := restoreQuiesceRecord{
		Version: 1, CommandID: "cmd_ok", JobID: quiesceTestJob, BackupID: quiesceTestBackup,
		Target: quiesceTestTarget, StopTimeoutSec: 60, Phase: "stopping",
	}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.Phase = "swapping"
	if err := bad.validate(); err == nil {
		t.Fatal("unknown phase accepted")
	}
	bad = valid
	bad.Phase = "starting"
	if err := bad.validate(); err == nil {
		t.Fatal("starting phase without outcome accepted")
	}
	bad = valid
	bad.Containers = []restoreQuiesceContainerPolicy{{ID: "../escape", Restart: "always"}}
	if err := bad.validate(); err == nil {
		t.Fatal("unsafe container identity accepted")
	}
	bad = valid
	bad.Containers = []restoreQuiesceContainerPolicy{{ID: strings.Repeat("a", 64), Restart: "unless-stopped-please"}}
	if err := bad.validate(); err == nil {
		t.Fatal("unknown restart policy accepted")
	}
	bad = valid
	bad.JobID = "../escape"
	if err := bad.validate(); err == nil {
		t.Fatal("unsafe journal identity accepted")
	}
}
