package executor

// X2 review, base-compatible control file: this test compiles against the
// pre-review commit d2eba960 and FAILS there (the base treats any Lstat
// error as "no Tor state"); it passes with the fix. Keep it free of any
// symbol the base does not have.

import (
	"context"
	"strings"
	"testing"
)

// Only a VERIFIED absence may skip the onion removal. A stat error on the
// state tree (here: an undecodable path component) is not "no state" — the
// cleanup must fail and be retried, not silently skip.
func TestUninstallExposureFailsWhenTorStateIsUnreadable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := NewDocker(t.TempDir(), discardLogger())
	d.Proxy = nil
	d.Tor.StateDir = d.Tor.StateDir + string(rune(0)) // Lstat fails with EINVAL, never ErrNotExist
	if err := d.removeDeploymentExposure(context.Background(), "dpl_neveronion"); err == nil {
		t.Fatal("a Tor state that could not be read was treated as absent")
	} else if !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("unreadable Tor state must surface as a verification failure, got: %v", err)
	}
}
