package proxy

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	transferSource = "dpl_aaaaaaaaaaaaaaaa"
	transferTarget = "dpl_bbbbbbbbbbbbbbbb"
)

var transferCutover = "fov_" + strings.Repeat("c", 24)

// liveIdentity installs a fresh identity as a live service, like a deploy.
func liveIdentity(t *testing.T, tor *Tor, id string) string {
	t.Helper()
	secret, public, addr, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tor.ImportOnionKeyWithProfile(id, secret, public, "hardened"); err != nil || got != addr {
		t.Fatalf("live identity: %v %s", err, got)
	}
	// Tor writes hostname once it loads the keys.
	if err := os.WriteFile(filepath.Join(tor.StateDir, "services", id, "hostname"), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return addr
}

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func assertNoService(t *testing.T, tor *Tor, id string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(tor.StateDir, "services", id)); !os.IsNotExist(err) {
		t.Fatalf("an identity was installed for %s (%v)", id, err)
	}
}

func TestOnionTransferMovesOneIdentityBetweenHosts(t *testing.T) {
	ctx := context.Background()
	source, target := newTestTor(t), newTestTor(t)
	addr := liveIdentity(t, source, transferSource)

	pub, err := target.PrepareOnionTransferRecipient(transferTarget, transferCutover, addr)
	if err != nil {
		t.Fatal(err)
	}
	again, err := target.PrepareOnionTransferRecipient(transferTarget, transferCutover, addr)
	if err != nil || *again != *pub {
		t.Fatalf("recipient is not stable for its cutover: %v", err)
	}
	if st, err := os.Stat(target.transferPath(transferTarget, transferCutover)); err != nil || (st.Mode().Perm()&0o077 != 0 && !strings.Contains(os.Getenv("OS"), "Windows")) {
		t.Fatalf("recipient private key is not private: %v", err)
	}

	if err := source.WithdrawHiddenService(ctx, transferSource, transferCutover, addr); err != nil {
		t.Fatal(err)
	}
	assertNoService(t, source, transferSource)
	torrc, err := os.ReadFile(filepath.Join(source.StateDir, "torrc"))
	if err != nil || strings.Contains(string(torrc), transferSource) {
		t.Fatalf("withdrawn service is still rendered into torrc: %v", err)
	}
	if err := source.WithdrawHiddenService(ctx, transferSource, transferCutover, addr); err != nil {
		t.Fatalf("repeating the withdrawal is not idempotent: %v", err)
	}

	sealed, err := source.ExportWithdrawnOnionKey(transferSource, transferCutover, addr, pub)
	if err != nil {
		t.Fatal(err)
	}
	got, err := target.ImportTransferredOnionKey(transferTarget, transferCutover, addr, b64(sealed), "hardened")
	if err != nil || got != addr {
		t.Fatalf("import: %v %s", err, got)
	}
	if target.profileOf(transferTarget) != ProfileHardened {
		t.Fatal("transferred identity lost its reviewed profile")
	}
	if _, err := os.Lstat(target.transferPath(transferTarget, transferCutover)); !os.IsNotExist(err) {
		t.Fatal("recipient private key survived a committed import")
	}
	// A lost acknowledgement retries the same activation after the recipient is gone.
	if got, err := target.ImportTransferredOnionKey(transferTarget, transferCutover, addr, b64(sealed), "hardened"); err != nil || got != addr {
		t.Fatalf("committed import is not idempotent: %v", err)
	}

	purged, err := source.PurgeOnionIdentity(ctx, transferSource, addr)
	if err != nil || len(purged) != 1 {
		t.Fatalf("source parked copy was not purged exactly once: %v %v", purged, err)
	}
	if _, _, got, err := keyDirAddress(filepath.Join(target.StateDir, "services", transferTarget)); err != nil || got != addr {
		t.Fatal("purging the source touched the target identity")
	}
	if err := source.EnsureWithdrawn(ctx, transferSource, transferCutover, addr); err != nil {
		t.Fatalf("startup check after purge must not fail: %v", err)
	}
}

func TestOnionTransferRefusesWrongRecipientCiphertextAndAddress(t *testing.T) {
	ctx := context.Background()
	source, target, stranger := newTestTor(t), newTestTor(t), newTestTor(t)
	addr := liveIdentity(t, source, transferSource)
	pub, err := target.PrepareOnionTransferRecipient(transferTarget, transferCutover, addr)
	if err != nil {
		t.Fatal(err)
	}
	strangerPub, err := stranger.PrepareOnionTransferRecipient(transferTarget, transferCutover, addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.WithdrawHiddenService(ctx, transferSource, transferCutover, addr); err != nil {
		t.Fatal(err)
	}
	forStranger, err := source.ExportWithdrawnOnionKey(transferSource, transferCutover, addr, strangerPub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.ImportTransferredOnionKey(transferTarget, transferCutover, addr, b64(forStranger), "hardened"); err == nil {
		t.Fatal("target opened a bundle sealed to another recipient")
	}
	sealed, err := source.ExportWithdrawnOnionKey(transferSource, transferCutover, addr, pub)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := target.ImportTransferredOnionKey(transferTarget, transferCutover, addr, b64(tampered), "hardened"); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	other := strings.Repeat("q", 56) + ".onion"
	if _, err := target.ImportTransferredOnionKey(transferTarget, transferCutover, other, b64(sealed), "hardened"); err == nil {
		t.Fatal("bundle was accepted for a different reviewed address")
	}
	otherCutover := "fov_" + strings.Repeat("d", 24)
	if _, err := target.PrepareOnionTransferRecipient(transferTarget, otherCutover, addr); err != nil {
		t.Fatal(err)
	}
	if _, err := target.ImportTransferredOnionKey(transferTarget, otherCutover, addr, b64(sealed), "hardened"); err == nil {
		t.Fatal("another cutover's recipient opened the bundle")
	}
	if _, err := target.ImportTransferredOnionKey(transferTarget, transferCutover, addr, b64(sealed)+"\n", "hardened"); err == nil {
		t.Fatal("non-canonical ciphertext encoding was accepted")
	}
	assertNoService(t, target, transferTarget)
	// The legitimate bundle still opens after every refusal.
	if got, err := target.ImportTransferredOnionKey(transferTarget, transferCutover, addr, b64(sealed), "hardened"); err != nil || got != addr {
		t.Fatalf("legitimate bundle refused after negatives: %v", err)
	}
}

func TestWithdrawRefusesAnotherIdentityAndUnverifiableState(t *testing.T) {
	ctx := context.Background()
	source := newTestTor(t)
	addr := liveIdentity(t, source, transferSource)
	other := strings.Repeat("q", 56) + ".onion"
	if err := source.WithdrawHiddenService(ctx, transferSource, transferCutover, other); err == nil {
		t.Fatal("withdrew a live identity other than the reviewed one")
	}
	if _, err := os.Lstat(filepath.Join(source.StateDir, "services", transferSource)); err != nil {
		t.Fatal("refused withdrawal still moved the live identity")
	}
	empty := newTestTor(t)
	if err := empty.WithdrawHiddenService(ctx, transferSource, transferCutover, addr); err == nil {
		t.Fatal("withdrawal succeeded without a live or parked identity")
	}
	if err := empty.EnsureWithdrawn(ctx, transferSource, transferCutover, addr); err != nil {
		t.Fatalf("nothing live means nothing to withdraw: %v", err)
	}
	if err := source.WithdrawHiddenService(ctx, transferSource, "fov_bad", addr); err == nil {
		t.Fatal("invalid cutover identity accepted")
	}
}

func TestPrivateOnionIsNeverTransferred(t *testing.T) {
	ctx := context.Background()
	source, target := newTestTor(t), newTestTor(t)
	addr := liveIdentity(t, source, transferSource)
	auth := filepath.Join(source.StateDir, "services", transferSource, "authorized_clients")
	if err := os.MkdirAll(auth, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auth, "alice.auth"), []byte("descriptor:x25519:AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pub, err := target.PrepareOnionTransferRecipient(transferTarget, transferCutover, addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.WithdrawHiddenService(ctx, transferSource, transferCutover, addr); err != nil {
		t.Fatalf("stopping publication is always allowed: %v", err)
	}
	if _, err := source.ExportWithdrawnOnionKey(transferSource, transferCutover, addr, pub); err == nil {
		t.Fatal("a private onion identity was sealed for transfer")
	}
}

func TestRecipientRefusesStandbyWithItsOwnIdentity(t *testing.T) {
	target := newTestTor(t)
	liveIdentity(t, target, transferTarget)
	if _, err := target.PrepareOnionTransferRecipient(transferTarget, transferCutover, strings.Repeat("p", 56)+".onion"); err == nil {
		t.Fatal("recipient created for a standby that already publishes an onion")
	}
	if _, err := target.PrepareOnionTransferRecipient(transferTarget, transferCutover, "not-an-onion"); err == nil {
		t.Fatal("invalid address accepted")
	}
}
