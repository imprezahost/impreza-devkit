package executor

import (
	"os"
	"path/filepath"
	"testing"
)

// A changed build request is refused before even a local-daemon probe runs.
func TestPreparationWorkerDriftRunsNoDockerAtAll(t *testing.T) {
	d, r, bin := workFixture(t, "build", "exit 0")
	calls := filepath.Join(t.TempDir(), "docker-calls")
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"$1\" >> '"+calls+"'\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dir, _, err := d.loadPreparationWork(r.Work, r.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	var request preparationWorkRequest
	if _, err := readWorkJSON(filepath.Join(dir, "request.json"), &request); err != nil {
		t.Fatal(err)
	}
	request.LocalBootRecovery = true
	if err := writeWorkJSON(dir, "request.json", request); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.appDir(r.DeploymentID), "compose.yaml"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunPreparationWorker(d.StateDir, r.Work.ID); err == nil {
		t.Fatal("changed inputs accepted")
	}
	if raw, err := os.ReadFile(calls); err == nil && len(raw) > 0 {
		t.Fatalf("docker ran before the input check: %q", raw)
	}
}
