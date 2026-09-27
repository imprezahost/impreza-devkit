package proxy

// An invalid hidden-service directory must never take the
// whole torrc — and with it every other onion on the host — down.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func torFixture(t *testing.T) *Tor {
	t.Helper()
	tor := NewTor(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := os.MkdirAll(filepath.Join(tor.StateDir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	return tor
}

func writeLiveService(t *testing.T, tor *Tor, id string) string {
	t.Helper()
	dir := filepath.Join(tor.StateDir, "services", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostname"), []byte("example.onion\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Name dimension: a directory nothing of ours created (a
// restore bind-mount, a manual copy) must not be rendered — quarantined.
func TestForeignServiceDirectoryIsQuarantinedNotRendered(t *testing.T) {
	tor := torFixture(t)
	live := writeLiveService(t, tor, "dpl_liveaaaaaaaaaaaaa")
	junk := filepath.Join(tor.StateDir, "services", "restore-junk")
	if err := os.MkdirAll(junk, 0700); err != nil {
		t.Fatal(err)
	}

	if err := tor.regenerateTorrc(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "services/dpl_liveaaaaaaaaaaaaa") {
		t.Fatal("live service dropped from torrc")
	}
	if strings.Contains(string(raw), "restore-junk") {
		t.Fatal("foreign directory rendered into torrc")
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatal("foreign directory kept in services/")
	}
	quarantined, err := os.ReadDir(filepath.Join(tor.StateDir, "quarantine"))
	if err != nil || len(quarantined) != 1 || !strings.HasPrefix(quarantined[0].Name(), "restore-junk-") {
		t.Fatalf("foreign directory not quarantined: %v %+v", err, quarantined)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live service directory disturbed", err)
	}
}

// Regression guard for a neighboring contract: corrupt profile metadata
// still fails the render instead of silently downgrading the tier.
func TestCorruptProfileStillFailsTorrcRender(t *testing.T) {
	tor := torFixture(t)
	dir := writeLiveService(t, tor, "dpl_corruptaaaaaaaaaaa")
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tor.regenerateTorrc(); err == nil {
		t.Fatal("corrupt profile metadata was accepted")
	}
}
