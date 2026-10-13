//go:build windows

package dockertest

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// This test binary acts as docker when started through the shim.
func init() { MaybeRun() }

// A metrics test failed now and then on Windows with the state "unknown"
// in under a second: a docker call could not save the double's state while
// another handle held state.json open (an antivirus scanning the file just
// written), and the half-second retry gave up. The call must outlast a
// holder that lets go after a moment.
func TestADockerCallOutlastsAHeldStateFile(t *testing.T) {
	d := Install(t, State{Containers: []Container{ComposeContainer("dpl_heldstate0000001", "web", "running", 0)}})
	held, err := os.Open(d.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(1500 * time.Millisecond)
		held.Close()
		close(released)
	}()
	defer func() { <-released }()
	out, err := exec.Command("docker", "ps", "-aq").CombinedOutput()
	if err != nil {
		t.Fatalf("a docker call failed while the state was held for a moment: %v: %s", err, out)
	}
	if calls := d.Calls("ps"); len(calls) != 1 {
		t.Fatalf("the call was not recorded: %d ps calls", len(calls))
	}
}

// Only the denial is retried: any other error returns at once.
func TestRetryingRenameDoesNotRetryOtherErrors(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	if err := RetryingRename(dir+`\missing`, dir+`\target`); err == nil {
		t.Fatal("renaming a missing file succeeded")
	} else if time.Since(start) > time.Second {
		t.Fatalf("a missing source was retried: %v", time.Since(start))
	}
}
