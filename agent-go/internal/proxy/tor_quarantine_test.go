package proxy

// Quarantine/ is for operator recovery,
// not forever — entries are time-stamped when quarantined and pruned past
// the same retention as parked keys; and a customer-confirmed purge also
// covers quarantined copies of the address.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quarantineFixture(t *testing.T, tor *Tor, name, hostname string) string {
	t.Helper()
	dir := filepath.Join(tor.StateDir, "quarantine", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if hostname != "" {
		if err := os.WriteFile(filepath.Join(dir, "hostname"), []byte(hostname+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestQuarantineIsPrunedPastRetention(t *testing.T) {
	tor := torFixture(t)
	old := quarantineFixture(t, tor, "dpl_oldaaaaaaaaaaaaa-deadbeef", "")
	past := time.Now().Add(-ParkRetention - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	// A fresh quarantine triggers the prune (same cadence as parked/).
	junk := filepath.Join(tor.StateDir, "services", "restore-junk")
	if err := os.MkdirAll(junk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tor.regenerateTorrc(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired quarantine entry was not pruned")
	}
	entries, err := os.ReadDir(filepath.Join(tor.StateDir, "quarantine"))
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "restore-junk-") {
		t.Fatalf("fresh quarantine entry lost or duplicated: %v %v", entries, err)
	}
}

func TestPurgeIncludesQuarantine(t *testing.T) {
	tor := newTestTor(t)
	target := strings.Repeat("p", 56) + ".onion"
	other := strings.Repeat("q", 56) + ".onion"
	parkedCopy := parkedFixture(t, tor, "dpl_purge", target)
	quarantinedCopy := quarantineFixture(t, tor, "dpl_purge-9f8e7d6c", target)
	otherQuarantined := quarantineFixture(t, tor, "dpl_purge-1a2b3c4d", other)

	purged, err := tor.PurgeOnionIdentity(context.Background(), "dpl_purge", target)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 2 {
		t.Fatalf("expected the parked AND quarantined copies purged, got %v", purged)
	}
	for _, dir := range []string{parkedCopy, quarantinedCopy} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("matching copy survived the purge: %s", dir)
		}
	}
	if _, err := os.Stat(otherQuarantined); err != nil {
		t.Fatal("purge destroyed a quarantined copy of another address", err)
	}
}
