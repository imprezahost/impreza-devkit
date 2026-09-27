package executor

// Uninstall on a host that never ran Tor must
// succeed; a failed FIRST onion deploy must unpublish its leftover
// hidden service.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// noTorContainer stubs the impreza_tor presence probe: this host has no
// Docker daemon, hence provably no container.
func noTorContainer(tor *proxy.Tor) {
	tor.ContainerPresent = func(context.Context) (bool, error) { return false, nil }
}

// With no Tor state at all, the exposure cleanup
// used to fail regenerating a torrc inside a tree that never existed.
func TestUninstallExposureWithoutTorStateIsSuccess(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := NewDocker(t.TempDir(), discardLogger())
	d.Proxy = nil
	noTorContainer(d.Tor)
	if err := d.removeDeploymentExposure(context.Background(), "dpl_neveronion"); err != nil {
		t.Fatalf("uninstall exposure cleanup failed on a host without Tor state: %v", err)
	}
}

// Only a VERIFIED absence may skip the onion removal. A stat
// error on the state tree is not "no state" (the base-compatible control
// lives in docker_uninstall_tor_state_test.go); here, a surviving
// impreza_tor container — even with every state directory gone — means
// the daemon may still hold services, so the cleanup must NOT skip.
func TestUninstallExposureDoesNotSkipWhileTorContainerExists(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := NewDocker(t.TempDir(), discardLogger())
	d.Proxy = nil
	d.Tor.ContainerPresent = func(context.Context) (bool, error) { return true, nil }
	err := d.removeDeploymentExposure(context.Background(), "dpl_neveronion")
	if err == nil {
		t.Fatal("with a live impreza_tor container the removal was skipped")
	}
	// The removal itself could not be verified without a Docker daemon —
	// that retryable failure is exactly the point: nothing was skipped.
	if !strings.Contains(err.Error(), "onion removal must be retried") {
		t.Fatalf("expected the retryable removal failure, got: %v", err)
	}
}

// A host WITH Tor state still goes through the full removal path.
func TestUninstallExposureWithTorStateStillReconciles(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := NewDocker(t.TempDir(), discardLogger())
	d.Proxy = nil
	if err := os.MkdirAll(filepath.Join(d.Tor.StateDir, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.Tor.StateDir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.removeDeploymentExposure(context.Background(), "dpl_nothingparked"); err != nil {
		// Without a Docker daemon (isolated PATH) the removal parks
		// nothing and only fails the daemon verification.
		if !strings.Contains(err.Error(), "Tor service removal") {
			t.Fatalf("unexpected exposure cleanup failure: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(d.Tor.StateDir, "torrc")); err != nil {
		t.Fatal("torrc was not regenerated", err)
	}
}

// A failed first deploy unpublishes its leftover hidden service —
// key material is parked for recovery, a directory WITHOUT keys (an
// initial-profile staging, a restore artifact) is deleted directly:
// parking it would block the customer purge later with "purge is not
// verified". A redeploy never touches the identity it owns.
func TestFailedFirstDeployOnionIsCleaned(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := NewDocker(t.TempDir(), discardLogger())
	d.Proxy = nil

	// No Tor state: cleanup is a clean no-op that creates nothing.
	if err := d.cleanupFailedFirstDeployOnion(context.Background(), "dpl_orphan", false); err != nil {
		t.Fatalf("cleanup failed without Tor state: %v", err)
	}
	if _, err := os.Stat(d.Tor.StateDir); !os.IsNotExist(err) {
		t.Fatal("cleanup created Tor state on a host without any")
	}

	// First deploy whose leftover service has keys and a published
	// hostname: the directory leaves services/ parked (the park rename
	// happens before the daemon verification, which fails here only
	// because Docker is absent).
	svc := filepath.Join(d.Tor.StateDir, "services", "dpl_orphan")
	if err := os.MkdirAll(svc, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc, "hs_ed25519_secret_key"), []byte("key-material"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc, "hostname"), []byte("example.onion\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.cleanupFailedFirstDeployOnion(context.Background(), "dpl_orphan", false); err != nil {
		if !strings.Contains(err.Error(), "Tor service removal") {
			t.Fatalf("unexpected cleanup failure: %v", err)
		}
	}
	if _, err := os.Stat(svc); !os.IsNotExist(err) {
		t.Fatal("orphan service directory kept in services/")
	}
	parked, err := os.ReadDir(filepath.Join(d.Tor.StateDir, "parked"))
	if err != nil || len(parked) != 1 {
		t.Fatalf("keys were not parked for recovery: %v %d", err, len(parked))
	}

	// First deploy whose leftover directory has NO key material (profile
	// staging only): deleted directly, nothing unidentifiable parked.
	svc2 := filepath.Join(d.Tor.StateDir, "services", "dpl_staging")
	if err := os.MkdirAll(svc2, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc2, "profile.json"), []byte(`{"profile":"standard"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.cleanupFailedFirstDeployOnion(context.Background(), "dpl_staging", false); err != nil {
		if !strings.Contains(err.Error(), "Tor service removal") {
			t.Fatalf("unexpected cleanup failure: %v", err)
		}
	}
	if _, err := os.Stat(svc2); !os.IsNotExist(err) {
		t.Fatal("keyless staging directory kept in services/")
	}
	parkedAfter, err := os.ReadDir(filepath.Join(d.Tor.StateDir, "parked"))
	if err != nil || len(parkedAfter) != 1 {
		t.Fatalf("a keyless directory must not be parked: %v %d", err, len(parkedAfter))
	}

	// A redeploy keeps its identity even after a failed attempt.
	svc3 := filepath.Join(d.Tor.StateDir, "services", "dpl_redeploy")
	if err := os.MkdirAll(svc3, 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.cleanupFailedFirstDeployOnion(context.Background(), "dpl_redeploy", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc3); err != nil {
		t.Fatal("redeploy onion identity was removed", err)
	}
}
