package proxy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"golang.org/x/crypto/nacl/box"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real public v3 address used as a known-good derivation vector (same one
// the Tor Manager's restore tests pin).
const ddgOnion = "duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion"

func ddgPub(t *testing.T) []byte {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(ddgOnion[:56]))
	if err != nil {
		t.Fatal(err)
	}
	return raw[:32]
}

// azFormSecret builds a C Tor-shaped secret file: padded header + 64-byte
// expanded scalar (content is fixture material — format is what's under
// test, Tor owns content validation).
func azFormSecret() []byte {
	az := bytes.Repeat([]byte{0x42}, 64)
	az[0] &= 248
	az[31] &= 127
	az[31] |= 64
	return append(append([]byte(secretKeyHeader), 0, 0, 0), az...)
}

func pubFileFor(pub []byte) []byte {
	return append(append([]byte(publicKeyHeader), 0, 0, 0), pub...)
}

func TestOnionAddressDerivation(t *testing.T) {
	addr, err := onionAddress(ddgPub(t))
	if err != nil {
		t.Fatal(err)
	}
	if addr != ddgOnion {
		t.Fatalf("derivation mismatch: %s != %s", addr, ddgOnion)
	}
}

func TestKeyFileValidation(t *testing.T) {
	pub := ddgPub(t)
	if err := ValidSecretKeyFile(azFormSecret()); err != nil {
		t.Fatal(err)
	}
	gotPub, addr, err := ParsePublicKeyFile(pubFileFor(pub))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPub, pub) || addr != ddgOnion {
		t.Fatal("pub file parse returned wrong pub/address")
	}
	// The public key is NOT read from the secret file: body[32:64] is the
	// nonce prefix, and treating it as a pubkey was the C4 live-test bug.
	if err := ValidSecretKeyFile(azFormSecret()[:95]); err == nil {
		t.Fatal("truncated secret accepted")
	}
	bad := pubFileFor(pub)
	bad[3] = 'X'
	if _, _, err := ParsePublicKeyFile(bad); err == nil {
		t.Fatal("bad pub header accepted")
	}
}

func TestImportExportCustody(t *testing.T) {
	tor := newTestTor(t)
	seed := bytes.Repeat([]byte{7}, 32)
	key := ed25519.NewKeyFromSeed(seed)
	pub := key.Public().(ed25519.PublicKey)
	expanded := sha512.Sum512(seed)
	expanded[0] &= 248
	expanded[31] &= 127
	expanded[31] |= 64
	secret := append(append([]byte(secretKeyHeader), 0, 0, 0), expanded[:]...)
	pubFile := pubFileFor(pub)
	wantAddr, _ := onionAddress(pub)

	addr, err := tor.ImportOnionKey("dpl_cust", secret, pubFile)
	if err != nil {
		t.Fatal(err)
	}
	if addr != wantAddr {
		t.Fatalf("import derived %s, want %s", addr, wantAddr)
	}
	svcDir := filepath.Join(tor.StateDir, "services", "dpl_cust")
	got, err := os.ReadFile(filepath.Join(svcDir, "hs_ed25519_secret_key"))
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatal("secret key not stored byte-exact")
	}
	gotPub, _ := os.ReadFile(filepath.Join(svcDir, "hs_ed25519_public_key"))
	if !bytes.Equal(gotPub, pubFile) {
		t.Fatal("public key file not stored byte-exact")
	}

	// Exact retry acknowledges the same durable identity without replacing it.
	if retry, err := tor.ImportOnionKey("dpl_cust", secret, pubFile); err != nil || retry != addr {
		t.Fatal("identical import retry refused", err)
	}
	otherSecret, otherPublic, _, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tor.ImportOnionKey("dpl_cust", otherSecret, otherPublic); err == nil {
		t.Fatal("import replaced an existing identity")
	}

	// Export seals to the recipient; the blob carries no plaintext.
	recipPub, recipSecret, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealed, addr, err := tor.ExportOnionKey("dpl_cust", recipPub)
	if err != nil {
		t.Fatal(err)
	}
	if addr != wantAddr {
		t.Fatal("export reported wrong address")
	}
	if bytes.Contains(sealed, secret[32:]) {
		t.Fatal("plaintext key material present in the sealed blob")
	}

	// Export refuses a missing service.
	if _, _, err := tor.ExportOnionKey("dpl_missing", recipPub); err == nil {
		t.Fatal("export of missing service accepted")
	}
	plain, ok := box.OpenAnonymous(nil, sealed, recipPub, recipSecret)
	if !ok {
		t.Fatal("cannot decrypt export")
	}
	var bundle struct {
		Version int    `json:"version"`
		Onion   string `json:"onion"`
		Secret  string `json:"secret_key_b64"`
		Public  string `json:"public_key_b64"`
	}
	if err := json.Unmarshal(plain, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Version != 1 || bundle.Onion != wantAddr {
		t.Fatal("export identity/version mismatch")
	}
	sec2, _ := base64.StdEncoding.DecodeString(bundle.Secret)
	pub2, _ := base64.StdEncoding.DecodeString(bundle.Public)
	other := newTestTor(t)
	addr2, err := other.ImportOnionKey("dpl_restored", sec2, pub2)
	if err != nil || addr2 != wantAddr {
		t.Fatalf("export cannot restore identity: %s %v", addr2, err)
	}
	var zero [32]byte
	if _, _, err := tor.ExportOnionKey("dpl_cust", &zero); err == nil {
		t.Fatal("low-order recipient accepted")
	}
	if _, err := tor.ImportOnionKey("dpl_badpair", secret, pubFileFor(ddgPub(t))); err == nil {
		t.Fatal("mismatched pair accepted")
	}
	if _, err := os.Stat(filepath.Join(tor.StateDir, "services", "dpl_badpair")); !os.IsNotExist(err) {
		t.Fatal("bad pair touched shared daemon state")
	}
	if _, err := tor.ImportOnionKey("../escape", secret, pubFile); err == nil {
		t.Fatal("path traversal import accepted")
	}
	if _, _, err := tor.ExportOnionKey("../escape", recipPub); err == nil {
		t.Fatal("path traversal export accepted")
	}
}
