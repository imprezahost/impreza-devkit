//go:build windows

package executor

import (
	"encoding/json"
	"syscall"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
)

// holdExclusive opens path with no sharing at all — what a docker shim (or
// an antivirus scan) briefly does on Windows — for the given time.
func holdExclusive(t *testing.T, path string, hold time.Duration) chan struct{} {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(hold)
		syscall.CloseHandle(h)
		close(released)
	}()
	return released
}

// The flapping watcher of TestStartupV2WindowResetsOnRestart read and
// replaced the double's state while the agent's own docker call held it,
// and failed with "being used by another process". A lock of a few hundred
// milliseconds is waited out, both for the read and for the replace.
func TestBumpRestartsWaitsOutABriefLock(t *testing.T) {
	id := "dpl_statelock0000000a"
	web := dockertest.ComposeContainer(id, "web", "running", 0)
	double := dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{web}})
	path := double.StatePath()

	released := holdExclusive(t, path, 300*time.Millisecond)
	bumpRestarts(t, path, id)
	<-released
	if t.Failed() {
		t.Fatal("a brief lock of the state file failed the restart bump")
	}
	raw, err := dockertest.ReadStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state dockertest.State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Containers) != 1 || state.Containers[0].RestartCount != 1 {
		t.Fatalf("the bump did not land after the lock was released: %+v", state.Containers)
	}

	// The double's own reads wait the same lock out.
	released = holdExclusive(t, path, 300*time.Millisecond)
	if _, ok := double.Container(web.Name[1:]); !ok {
		t.Fatal("the double could not read its state through a brief lock")
	}
	<-released
}
