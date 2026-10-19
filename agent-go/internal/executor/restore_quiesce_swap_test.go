package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
)

// A scripted rewrite that already read the state must not put back a restart
// policy the shim saved in that window. That clobber is what fails the
// lying-stop abort when the package is busy on Windows.
func TestQuiesceExitRewriteKeepsAPolicySavedWhileItRuns(t *testing.T) {
	web := dockertest.ComposeContainer(quiesceTestTarget, "app", "running", 0)
	web.HostConfig.RestartPolicy.Name = "no"
	job := dockertest.ComposeContainer(quiesceTestJob, "job", "running", 0)
	path := filepath.Join(t.TempDir(), "state.json")
	raw, err := json.Marshal(dockertest.State{Containers: []dockertest.Container{web, job}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	quiesceStateReadHook = func(p string) {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		var state dockertest.State
		if json.Unmarshal(body, &state) != nil {
			t.Error("decode")
			return
		}
		for i := range state.Containers {
			if state.Containers[i].Config.Labels["com.docker.compose.project"] == quiesceTestTarget {
				state.Containers[i].HostConfig.RestartPolicy.Name = "always"
			}
		}
		out, err := json.Marshal(state)
		if err != nil {
			t.Errorf("encode: %v", err)
			return
		}
		if err := os.WriteFile(p, out, 0o600); err != nil {
			t.Errorf("write: %v", err)
		}
	}
	t.Cleanup(func() { quiesceStateReadHook = nil })

	quiesceExitJob(t, path, 1)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state dockertest.State
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	var policy string
	var jobExited bool
	for _, c := range state.Containers {
		switch c.Config.Labels["com.docker.compose.project"] {
		case quiesceTestTarget:
			policy = c.HostConfig.RestartPolicy.Name
		case quiesceTestJob:
			jobExited = c.State.Status == "exited" && c.State.ExitCode == 1
		}
	}
	if policy != "always" || !jobExited {
		t.Fatalf("rewrite put back a stale snapshot: policy=%q jobExited=%v", policy, jobExited)
	}
}
