//go:build linux

package proxy

// Mode dimension (Linux only: permission bits are real here). A
// permissive directory is repaired when it carries agent-managed state
// and quarantined when it is empty — Tor 0.4.9 would otherwise refuse
// the whole configuration.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The measured restore artifact: Docker creates the missing bind-mount source
// empty, root:root, 0755 — and Tor refuses to start with it in the torrc.
//
// The empty artifact carries nothing to preserve, so it is NOT
// quarantined (a restore racing an agent restart may still be about to
// fill it). It stays in services/, out of the torrc, and the next
// provisioning of the same deployment repairs the mode and heals it.
func TestPermissiveEmptyServiceDirectoryStaysUnrenderedInPlace(t *testing.T) {
	tor := torFixture(t)
	writeLiveService(t, tor, "dpl_liveaaaaaaaaaaaaa")
	junk := filepath.Join(tor.StateDir, "services", "dpl_junkaaaaaaaaaaaaa")
	if err := os.Mkdir(junk, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(junk, 0755); err != nil {
		t.Fatal(err)
	}

	if err := tor.regenerateTorrc(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "dpl_junkaaaaaaaaaaaaa") {
		t.Fatal("permissive empty directory rendered into torrc")
	}
	if !strings.Contains(string(raw), "services/dpl_liveaaaaaaaaaaaaa") {
		t.Fatal("live service dropped from torrc over a neighbor's bad mode")
	}
	if _, err := os.Stat(junk); err != nil {
		t.Fatal("empty restore artifact was moved away instead of left in place", err)
	}
	if entries, err := os.ReadDir(filepath.Join(tor.StateDir, "quarantine")); err == nil && len(entries) > 0 {
		t.Fatal("an empty directory with nothing to preserve was quarantined")
	}
}

// A permissive directory that carries agent-managed state is repaired,
// never quarantined: unpublishing a live onion over a mode bit would be
// its own outage.
func TestPermissiveStatefulServiceDirectoryIsRepaired(t *testing.T) {
	tor := torFixture(t)
	dir := writeLiveService(t, tor, "dpl_permAAAAAAAAAAAAA")
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte(`{"profile":"standard"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := tor.regenerateTorrc(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode not repaired: %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "services/dpl_permAAAAAAAAAAAAA") {
		t.Fatal("repaired service dropped from torrc")
	}
	if _, err := os.ReadDir(filepath.Join(tor.StateDir, "quarantine")); !os.IsNotExist(err) {
		t.Fatal("stateful directory was quarantined instead of repaired")
	}
}
