//go:build windows

package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
)

// On Windows a file just written is often held open for a moment by the
// antivirus, and replacing it by rename is then denied ("Access is
// denied"): TestOnionPolicyInterruptedUpdateRecoversBeforeNewMutation
// failed now and then with the policy journal's commit refused that way.
// Linux, where the agent runs, never denies it and keeps os.Rename.
func init() { torRename = dockertest.RetryingRename }

// The denial, made on purpose: another handle holds the target open, the
// way the scanner does, and lets go after a moment. The write must wait for
// it instead of failing the commit.
func TestTorPrivateWriteOutlastsAHeldTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy-change.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
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
	if err := torPrivateWrite(path, []byte(`{"version":1,"committed":true}`)); err != nil {
		t.Fatalf("a target held open for a moment failed the write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"version":1,"committed":true}` {
		t.Fatalf("the write did not land: %q %v", got, err)
	}
}
