package proxy

// Quarantine/ joined the purge, but a quarantined key set Tor never
// published has no hostname, so the purge of its deployment failed with
// "purge is not verified" for the 30-day retention — the parking failure
// again, through quarantine. The purge now identifies a copy by its key
// pair (the hostname file is Tor-writable), falls back to the hostname only
// without a verifiable pair, leaves a directory with no identity at all to
// retention, and identifies every copy before deleting any.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeKeyPair(t *testing.T, dir string, secret, public []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"hs_ed25519_secret_key": secret, "hs_ed25519_public_key": public} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPurgeIdentifiesQuarantinedKeySetWithoutHostname(t *testing.T) {
	tor := newTestTor(t)
	secret, public, addr, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tor.StateDir, "quarantine", "dpl_q-0011223344556677")
	writeKeyPair(t, dir, secret, public)

	purged, err := tor.PurgeOnionIdentity(context.Background(), "dpl_q", addr)
	if err != nil {
		t.Fatalf("a quarantined key set without hostname blocks the purge: %v", err)
	}
	if len(purged) != 1 {
		t.Fatalf("expected the quarantined copy purged, got %v", purged)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("quarantined key material survived the confirmed purge")
	}
}

func TestPurgeLeavesAStrayWithoutIdentityToRetention(t *testing.T) {
	tor := newTestTor(t)
	secret, public, addr, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(tor.StateDir, "parked", "dpl_q")
	writeKeyPair(t, parked, secret, public)
	if err := os.WriteFile(filepath.Join(parked, "hostname"), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A quarantined non-deployment name under this prefix: no key, no
	// hostname — nothing of any address to destroy.
	stray := quarantineFixture(t, tor, "dpl_q-junk-aabbccdd", "")
	if err := os.WriteFile(filepath.Join(stray, "notes"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	purged, err := tor.PurgeOnionIdentity(context.Background(), "dpl_q", addr)
	if err != nil {
		t.Fatalf("a stray directory without identity blocks the purge: %v", err)
	}
	if len(purged) != 1 {
		t.Fatalf("expected the parked copy purged, got %v", purged)
	}
	if _, err := os.Stat(parked); !os.IsNotExist(err) {
		t.Fatal("parked copy survived the purge")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("the purge deleted a directory it could not attribute to the address", err)
	}
}

// Hosts upgraded from an older agent may still carry a parked
// staging with only its profile: no key, nothing to destroy.
func TestPurgeLeavesALegacyProfileOnlyParkedStaging(t *testing.T) {
	tor := newTestTor(t)
	secret, public, addr, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(tor.StateDir, "parked", "dpl_q")
	writeKeyPair(t, parked, secret, public)
	if err := os.WriteFile(filepath.Join(parked, "hostname"), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(tor.StateDir, "parked", "dpl_q-00112233aabbccdd")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "profile.json"), []byte(`{"profile":"standard"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	purged, err := tor.PurgeOnionIdentity(context.Background(), "dpl_q", addr)
	if err != nil {
		t.Fatalf("a legacy profile-only parked staging blocks the purge: %v", err)
	}
	if len(purged) != 1 {
		t.Fatalf("expected the parked copy purged, got %v", purged)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatal("the purge deleted the keyless staging it could not attribute", err)
	}
}

func TestPurgeIdentifiesByKeyPairNotByAForgedHostname(t *testing.T) {
	tor := newTestTor(t)
	secret, public, addr, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	_, _, other, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(tor.StateDir, "parked", "dpl_q")
	writeKeyPair(t, parked, secret, public)
	// The hostname file is written where the Tor container can write.
	if err := os.WriteFile(filepath.Join(parked, "hostname"), []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	purged, err := tor.PurgeOnionIdentity(context.Background(), "dpl_q", other)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 0 {
		t.Fatalf("a forged hostname made the purge of another address destroy this key set: %v", purged)
	}
	purged, err = tor.PurgeOnionIdentity(context.Background(), "dpl_q", addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 {
		t.Fatalf("the key set of the confirmed address survived behind a forged hostname: %v", purged)
	}
}

func TestPurgeThatCannotIdentifyACopyDeletesNothing(t *testing.T) {
	tor := newTestTor(t)
	secret, public, addr, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	// An identifiable parked copy (processed first) ...
	parked := filepath.Join(tor.StateDir, "parked", "dpl_q")
	writeKeyPair(t, parked, secret, public)
	if err := os.WriteFile(filepath.Join(parked, "hostname"), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ... and key material that cannot be attributed: a secret key whose
	// public file is damaged, and no hostname.
	damaged := filepath.Join(tor.StateDir, "quarantine", "dpl_q-8899aabbccddeeff")
	writeKeyPair(t, damaged, secret, public[:10])

	if _, err := tor.PurgeOnionIdentity(context.Background(), "dpl_q", addr); err == nil {
		t.Fatal("a purge that cannot identify retained key material reported success")
	}
	if _, err := os.Stat(parked); err != nil {
		t.Fatal("the failed purge already deleted a copy (partial delete)", err)
	}
	if _, err := os.Stat(damaged); err != nil {
		t.Fatal("the failed purge deleted unattributed key material", err)
	}
}
