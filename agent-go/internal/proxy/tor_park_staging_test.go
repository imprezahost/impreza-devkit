package proxy

// Parking a service whose keys were staged but never published
// (a failed first deploy, an import written before provisioning) must write
// the hostname derived from the public key BEFORE the rename — otherwise the
// later customer-confirmed purge fails with "purge is not verified" and the
// key material sits on disk until retention. A directory with no key
// material is not an identity at all: it is deleted directly, never parked.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParkingDerivesHostnameFromTheKeyPair(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()

	dir := filepath.Join(tor.StateDir, "services", "dpl_staged")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret, public, addr, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	// Keys staged, hostname never published (Tor never saw this service).
	if err := os.WriteFile(filepath.Join(dir, "hs_ed25519_secret_key"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hs_ed25519_public_key"), public, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := tor.RemoveHiddenService(ctx, "dpl_staged"); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(tor.StateDir, "parked", "dpl_staged")
	raw, err := os.ReadFile(filepath.Join(parked, "hostname"))
	if err != nil {
		t.Fatalf("parked identity has no hostname — the purge cannot verify it later: %v", err)
	}
	if strings.TrimSpace(string(raw)) != addr {
		t.Fatalf("parked hostname %q does not match the derived address %q", strings.TrimSpace(string(raw)), addr)
	}

	// The exact failure chain from the review: the customer-confirmed
	// purge of that address must succeed against the parked copy.
	purged, err := tor.PurgeOnionIdentity(ctx, "dpl_staged", addr)
	if err != nil {
		t.Fatalf("purge blocked after parking without a published hostname: %v", err)
	}
	if len(purged) != 1 {
		t.Fatalf("expected the parked copy purged, got %v", purged)
	}
	if _, err := os.Stat(parked); !os.IsNotExist(err) {
		t.Fatal("purged copy survived")
	}
}

func TestKeylessStagingDirectoryIsDeletedNotParked(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()

	// PrepareInitialProfile's staging shape: metadata, no keys.
	dir := filepath.Join(tor.StateDir, "services", "dpl_profileonly")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte("{\"profile\":\"standard\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := tor.RemoveHiddenService(ctx, "dpl_profileonly"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("keyless directory kept in services/")
	}
	parked := filepath.Join(tor.StateDir, "parked")
	entries, err := os.ReadDir(parked)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "dpl_profileonly") {
			t.Fatal("a directory with no key material was parked; it would block the purge later")
		}
	}
}

// A present-but-unreadable key pair must NOT be deleted by the new
// keyless branch, nor parked unidentifiable: the removal fails and is
// retried.
func TestUnverifiableKeyPairIsNotDeletedNorParked(t *testing.T) {
	tor := newTestTor(t)
	ctx := context.Background()

	dir := filepath.Join(tor.StateDir, "services", "dpl_badkeys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hs_ed25519_secret_key"), []byte("not a real key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hs_ed25519_public_key"), []byte("not a real key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := tor.RemoveHiddenService(ctx, "dpl_badkeys"); err == nil {
		t.Fatal("an unverifiable key pair was parked or deleted silently")
	}
	if _, err := os.Stat(filepath.Join(dir, "hs_ed25519_secret_key")); err != nil {
		t.Fatal("unverifiable key material was destroyed", err)
	}
}
