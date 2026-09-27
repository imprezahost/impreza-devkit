//go:build linux

package proxy

// A compromised Tor container with a
// writable services/ mount could replace a service directory with a symlink
// and have the agent — root — apply 0700 to an arbitrary host directory.
// The provisioning chmod must validate the chain and work by descriptor
// with O_NOFOLLOW, never following a symlink.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProvisionHiddenServiceNeverChmodsThroughSymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mode enforcement and symlink semantics are Linux-only here")
	}
	tor := torFixture(t)
	// No Docker daemon: provisioning must refuse BEFORE touching it anyway.
	t.Setenv("PATH", t.TempDir())

	target := filepath.Join(t.TempDir(), "hostdir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tor.StateDir, "services", "dpl_swap")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := tor.ProvisionHiddenService(context.Background(), "dpl_swap", 80)
	if err == nil {
		t.Fatal("provisioning accepted a symlinked service directory")
	}
	info, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("chmod followed the symlink into the host directory: mode now %04o", info.Mode().Perm())
	}
}

// The repair path inside the torrc render follows the same rule.
func TestRepairNeverChmodsThroughSymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mode enforcement and symlink semantics are Linux-only here")
	}
	tor := torFixture(t)
	writeLiveService(t, tor, "dpl_liveaaaaaaaaaaaaa")

	target := filepath.Join(t.TempDir(), "hostdir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink that even carries "state" behind the link: it must be
	// excluded, never repaired through the link.
	realDir := filepath.Join(t.TempDir(), "realstate")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "hostname"), []byte("x.onion\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tor.StateDir, "services", "dpl_swaprepair")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	if err := tor.regenerateTorrc(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "dpl_swaprepair") {
		t.Fatal("a symlinked service directory was rendered into the torrc")
	}
	if !strings.Contains(string(raw), "dpl_liveaaaaaaaaaaaaa") {
		t.Fatal("live service dropped over a symlinked neighbor")
	}
}
