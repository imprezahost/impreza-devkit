package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parkedFixture creates a parked copy with the given hostname and returns
// its directory.
func parkedFixture(t *testing.T, tor *Tor, name, hostname string) string {
	t.Helper()
	dir := filepath.Join(tor.StateDir, "parked", name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string]string{
		"hs_ed25519_secret_key": "secret-of-" + name,
		"hs_ed25519_public_key": "public-of-" + name,
		"hostname":              hostname + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPurgeOnionIdentityDeletesOnlyMatchingCopies(t *testing.T) {
	tor := newTestTor(t)
	target := strings.Repeat("p", 56) + ".onion"
	other := strings.Repeat("q", 56) + ".onion"
	rotCopy := parkedFixture(t, tor, "dpl_purge-cmd_rot1", target)
	uninstallCopy := parkedFixture(t, tor, "dpl_purge", target)
	otherAddress := parkedFixture(t, tor, "dpl_purge-cmd_rot2", other)
	otherDeployment := parkedFixture(t, tor, "dpl_other-cmd_rot1", target)

	purged, err := tor.PurgeOnionIdentity(context.Background(), "dpl_purge", target)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 2 {
		t.Fatalf("expected the two matching copies, got %v", purged)
	}
	for _, dir := range []string{rotCopy, uninstallCopy} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("matching parked copy survived the purge")
		}
	}
	for _, dir := range []string{otherAddress, otherDeployment} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("purge destroyed unrelated material: %s (%v)", dir, err)
		}
	}
	// Purge is idempotent: the guarantee already holds.
	purged, err = tor.PurgeOnionIdentity(context.Background(), "dpl_purge", target)
	if err != nil || len(purged) != 0 {
		t.Fatalf("repeat purge not a clean no-op: %v %v", purged, err)
	}
}

func TestPurgeOnionIdentityRefusesTheActiveIdentity(t *testing.T) {
	tor := newTestTor(t)
	svc := filepath.Join(tor.StateDir, "services", "dpl_live")
	if err := os.MkdirAll(svc, 0700); err != nil {
		t.Fatal(err)
	}
	live := strings.Repeat("l", 56) + ".onion"
	if err := os.WriteFile(filepath.Join(svc, "hostname"), []byte(live+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	copyDir := parkedFixture(t, tor, "dpl_live-cmd_rot1", live)
	if _, err := tor.PurgeOnionIdentity(context.Background(), "dpl_live", live); err == nil {
		t.Fatal("active identity accepted for purge")
	}
	if _, err := os.Stat(filepath.Join(svc, "hostname")); err != nil {
		t.Fatal("live identity touched")
	}
	if _, err := os.Stat(copyDir); err != nil {
		t.Fatal("parked copy touched while refusing an active identity")
	}
}

func TestPurgeOnionIdentityValidatesInputs(t *testing.T) {
	tor := newTestTor(t)
	for _, tc := range [][2]string{
		{"bad id", strings.Repeat("p", 56) + ".onion"},
		{"dpl_valid", "not-an-onion"},
		{"dpl_valid", strings.Repeat("p", 55) + ".onion"},
	} {
		if _, err := tor.PurgeOnionIdentity(context.Background(), tc[0], tc[1]); err == nil {
			t.Fatalf("invalid input accepted: %q / %q", tc[0], tc[1])
		}
	}
}

// A parked directory without a hostname is not an identity the purge can
// vouch for — it must be left alone, not blanket-deleted.
func TestPurgeOnionIdentityRefusesUnidentifiableDirectories(t *testing.T) {
	tor := newTestTor(t)
	target := strings.Repeat("p", 56) + ".onion"
	mystery := filepath.Join(tor.StateDir, "parked", "dpl_purge-unknown")
	if err := os.MkdirAll(mystery, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mystery, "stray"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tor.PurgeOnionIdentity(context.Background(), "dpl_purge", target); err == nil {
		t.Fatal("unidentifiable retained state reported as purged")
	}
	if _, err := os.Stat(filepath.Join(mystery, "stray")); err != nil {
		t.Fatal("purge deleted a directory it could not identify")
	}
}
