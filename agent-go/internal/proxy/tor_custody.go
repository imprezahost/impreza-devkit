package proxy

// Onion key custody: import of an existing hidden-service key
// (bring your own onion), export sealed to the customer's own public key, and
// rotation with the old key parked for recovery.
//
// Key formats are C Tor's on-disk ones — verified against a live daemon:
//
//   hs_ed25519_secret_key = "== ed25519v1-secret: type0 ==" + NUL-pad to 32
//                         + 64-byte expanded secret (az = SHA-512(seed),
//                         first half clamped). The PUBLIC KEY IS NOT IN THIS
//                         FILE — body[32:64] is the nonce prefix. (Writing a
//                         seed‖pub file here is refused by Tor with "Error
//                         loading rendezvous service keys".)
//   hs_ed25519_public_key = "== ed25519v1-public: type0 ==" + NUL-pad to 32
//                         + 32-byte public key. The address always derives
//                         from THIS file; the agent also derives the public
//                         point from the scalar and checks their correspondence.
//   address = base32(pub || checksum || 0x03)[:56] + ".onion"
//   checksum = SHA3-256(".onion checksum" || pub || 0x03)[:2]
//
// Export seals a key-pair bundle to the customer's X25519 public key agent-side.
// Import reaches the authenticated API over its secure transport; the control
// plane encrypts the queued material before storing it and decrypts it at claim.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/sha3"
)

const (
	secretKeyHeader = "== ed25519v1-secret: type0 ==" // + NUL padding to 32 bytes
	publicKeyHeader = "== ed25519v1-public: type0 ==" // + NUL padding to 32 bytes
	headerPaddedLen = 32
)

// paddedHeader validates the C Tor key-file framing: the ASCII header
// followed by NUL padding up to 32 bytes.
func paddedHeader(data []byte, header string) bool {
	if len(data) < headerPaddedLen {
		return false
	}
	if string(data[:len(header)]) != header {
		return false
	}
	for _, b := range data[len(header):headerPaddedLen] {
		if b != 0 {
			return false
		}
	}
	return true
}

// onionAddress derives the v3 address for a raw 32-byte ed25519 public key.
func onionAddress(pub []byte) (string, error) {
	if len(pub) != 32 {
		return "", fmt.Errorf("ed25519 public key must be 32 bytes, got %d", len(pub))
	}
	sum := sha3.Sum256(append(append([]byte(".onion checksum"), pub...), 0x03))
	raw := append(append([]byte{}, pub...), sum[0], sum[1], 0x03)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)) + ".onion", nil
}

// ValidSecretKeyFile reports whether data has the exact C Tor
// hs_ed25519_secret_key framing (32-byte padded header + 64-byte expanded
// secret) and the expanded scalar's required clamping bits.
func ValidSecretKeyFile(data []byte) error {
	if len(data) != headerPaddedLen+64 {
		return fmt.Errorf("secret key file must be %d bytes, got %d", headerPaddedLen+64, len(data))
	}
	if !paddedHeader(data, secretKeyHeader) {
		return fmt.Errorf("not an hs_ed25519_secret_key file (bad header)")
	}
	if data[32]&7 != 0 || data[63]&0xc0 != 0x40 {
		return fmt.Errorf("expanded secret scalar is not clamped")
	}
	return nil
}

var onionDeploymentID = regexp.MustCompile(`^dpl_[a-zA-Z0-9_-]{1,28}$`)

// Validate the expanded scalar against the full compressed public point before
// touching the shared daemon. A malformed import must not break other services.
func validateOnionKeyPair(secret, public []byte) (string, error) {
	if err := ValidSecretKeyFile(secret); err != nil {
		return "", err
	}
	pub, addr, err := ParsePublicKeyFile(public)
	if err != nil {
		return "", err
	}
	scalar, err := new(edwards25519.Scalar).SetBytesWithClamping(secret[32:64])
	if err != nil {
		return "", err
	}
	derived := new(edwards25519.Point).ScalarBaseMult(scalar).Bytes()
	if !bytes.Equal(pub, derived) {
		return "", fmt.Errorf("hidden-service secret and public keys do not match")
	}
	return addr, nil
}

// ParsePublicKeyFile validates a C Tor hs_ed25519_public_key file and
// returns the embedded key and the v3 address it publishes.
func ParsePublicKeyFile(data []byte) (pub []byte, addr string, err error) {
	if len(data) != headerPaddedLen+32 {
		return nil, "", fmt.Errorf("public key file must be %d bytes, got %d", headerPaddedLen+32, len(data))
	}
	if !paddedHeader(data, publicKeyHeader) {
		return nil, "", fmt.Errorf("not an hs_ed25519_public_key file (bad header)")
	}
	pub = append([]byte{}, data[headerPaddedLen:]...)
	addr, err = onionAddress(pub)
	if err != nil {
		return nil, "", err
	}
	return pub, addr, nil
}

// ImportOnionKey writes a customer-supplied key pair into the service
// directory BEFORE Tor provisions the service, so the deployment publishes
// the customer's existing address. Both files are required: the address
// derives from the PUBLIC file; the agent verifies the pair before writing.
// The caller (control plane) has already
// refused address duplicates across tenants. Returns the derived address.
func (t *Tor) ImportOnionKey(deploymentID string, secretFile, pubFile []byte) (string, error) {
	return t.ImportOnionKeyWithProfile(deploymentID, secretFile, pubFile, "standard")
}

// Stage identity and policy together so startup cannot publish an imported
// identity with a weaker default after an interrupted deploy.
func (t *Tor) ImportOnionKeyWithProfile(deploymentID string, secretFile, pubFile []byte, profile string) (string, error) {
	if !validOnionProfile(profile) {
		return "", fmt.Errorf("invalid initial onion profile")
	}
	if err := t.guardRotation(); err != nil {
		return "", err
	}
	if !onionDeploymentID.MatchString(deploymentID) {
		return "", fmt.Errorf("invalid deployment ID")
	}
	addr, err := validateOnionKeyPair(secretFile, pubFile)
	if err != nil {
		return "", err
	}
	svcDir := filepath.Join(t.StateDir, "services", deploymentID)
	if _, err := os.Lstat(svcDir); err == nil {
		if _, err := t.serviceDir(deploymentID); err != nil {
			return "", err
		}
		// A lost acknowledgement or interrupted deploy may retry the same
		// imported identity. It must not replace keys or change their policy.
		for _, file := range []struct {
			name     string
			expected []byte
		}{{"hs_ed25519_secret_key", secretFile}, {"hs_ed25519_public_key", pubFile}} {
			path := filepath.Join(svcDir, file.name)
			st, err := os.Lstat(path)
			if err != nil || !st.Mode().IsRegular() || st.Size() != int64(len(file.expected)) {
				return "", fmt.Errorf("stored onion identity requires recovery")
			}
			stored, err := os.ReadFile(path)
			if err != nil || subtle.ConstantTimeCompare(stored, file.expected) != 1 {
				return "", fmt.Errorf("deployment already has a different onion identity")
			}
		}
		current, err := t.readProfile(deploymentID)
		if err != nil || string(current) != profile {
			return "", fmt.Errorf("stored onion profile differs from the import request")
		}
		return addr, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := t.ensureDirs(); err != nil {
		return "", err
	}
	// Stage outside services/: a partial import is never rendered into torrc.
	staging, err := os.MkdirTemp(t.StateDir, "import-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := writeInitialOnionProfile(staging, profile); err != nil {
		return "", err
	}
	if err := torPrivateWrite(filepath.Join(staging, "hs_ed25519_secret_key"), secretFile); err != nil {
		return "", fmt.Errorf("write imported secret key: %w", err)
	}
	if err := torPrivateWrite(filepath.Join(staging, "hs_ed25519_public_key"), pubFile); err != nil {
		return "", fmt.Errorf("write imported public key: %w", err)
	}
	if err := os.Rename(staging, svcDir); err != nil {
		return "", fmt.Errorf("install imported key pair: %w", err)
	}
	if err := syncRoutingDir(filepath.Dir(svcDir)); err != nil {
		return "", err
	}
	t.Log.Info("proxy/tor: imported hidden-service key", "deployment_id", deploymentID, "onion", addr)
	return addr, nil
}

// ExportOnionKey seals the deployment's hidden-service secret key to the
// customer's X25519 public key (NaCl anonymous box). The plaintext never
// leaves the host unencrypted; the control plane sees only the sealed blob.
func (t *Tor) ExportOnionKey(deploymentID string, recipientPub *[32]byte) ([]byte, string, error) {
	if err := t.guardRotation(); err != nil {
		return nil, "", err
	}
	if !onionDeploymentID.MatchString(deploymentID) || recipientPub == nil {
		return nil, "", fmt.Errorf("invalid export identity")
	}
	recipient, err := ecdh.X25519().NewPublicKey(recipientPub[:])
	if err != nil {
		return nil, "", fmt.Errorf("invalid export recipient")
	}
	probe, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	if _, err := probe.ECDH(recipient); err != nil {
		return nil, "", fmt.Errorf("invalid export recipient")
	}
	svcDir, err := t.serviceDir(deploymentID)
	if err != nil {
		return nil, "", err
	}
	for _, name := range []string{"hs_ed25519_secret_key", "hs_ed25519_public_key"} {
		st, err := os.Lstat(filepath.Join(svcDir, name))
		if err != nil || !st.Mode().IsRegular() {
			return nil, "", fmt.Errorf("unsafe stored onion key")
		}
	}
	secret, err := os.ReadFile(filepath.Join(svcDir, "hs_ed25519_secret_key"))
	if err != nil {
		return nil, "", fmt.Errorf("no hidden-service key for deployment %s", deploymentID)
	}
	if err := ValidSecretKeyFile(secret); err != nil {
		return nil, "", fmt.Errorf("stored secret key unreadable: %w", err)
	}
	public, err := os.ReadFile(filepath.Join(svcDir, "hs_ed25519_public_key"))
	if err != nil {
		return nil, "", err
	}
	addr, err := validateOnionKeyPair(secret, public)
	if err != nil {
		return nil, "", err
	}
	bundle, err := json.Marshal(struct {
		Version int    `json:"version"`
		Onion   string `json:"onion"`
		Secret  string `json:"secret_key_b64"`
		Public  string `json:"public_key_b64"`
	}{1, addr, base64.StdEncoding.EncodeToString(secret), base64.StdEncoding.EncodeToString(public)})
	if err != nil {
		return nil, "", err
	}
	sealed, err := box.SealAnonymous(nil, bundle, recipientPub, rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("seal onion key: %w", err)
	}
	t.Log.Info("proxy/tor: hidden-service key exported (sealed)", "deployment_id", deploymentID, "onion", addr)
	return sealed, addr, nil
}
