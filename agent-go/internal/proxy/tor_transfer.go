package proxy

// Onion identity transfer for a reviewed cross-host failover (onion-transfer-v1).
//
// The target creates a one-cutover X25519 recipient and reports only its
// public half. The source's fence withdraws the hidden service: the key set
// moves to parked/<deployment>-<cutover>, so Tor stops publishing it, and that
// parked copy is sealed to the recipient. The target opens the bundle with the
// private half, imports it as its own identity and deletes the recipient. The
// control plane relays ciphertext only. The key exists on both hosts between
// withdrawal and the source's purge of its parked copy.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"golang.org/x/crypto/nacl/box"
)

var transferCutoverID = regexp.MustCompile(`^fov_[a-f0-9]{24}$`)

// TransferRetention bounds how long an unused recipient private key survives
// on the target when a cutover never delivers its bundle.
const TransferRetention = 7 * 24 * time.Hour

type onionTransferRecipient struct {
	Version    int    `json:"version"`
	Deployment string `json:"deployment"`
	Cutover    string `json:"cutover"`
	Onion      string `json:"onion"`
	Private    []byte `json:"private"`
}

type onionKeyBundle struct {
	Version int    `json:"version"`
	Onion   string `json:"onion"`
	Secret  string `json:"secret_key_b64"`
	Public  string `json:"public_key_b64"`
}

func validTransferIdentity(deploymentID, cutoverID, onion string) error {
	if !onionDeploymentID.MatchString(deploymentID) || !transferCutoverID.MatchString(cutoverID) ||
		!onionAddressRe.MatchString(onion) {
		return fmt.Errorf("invalid onion transfer identity")
	}
	return nil
}

// sealOnionKeyBundle is the export format shared with ExportOnionKey.
func sealOnionKeyBundle(secret, public []byte, addr string, recipientPub *[32]byte) ([]byte, error) {
	if recipientPub == nil {
		return nil, fmt.Errorf("invalid export recipient")
	}
	recipient, err := ecdh.X25519().NewPublicKey(recipientPub[:])
	if err != nil {
		return nil, fmt.Errorf("invalid export recipient")
	}
	probe, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if _, err := probe.ECDH(recipient); err != nil {
		return nil, fmt.Errorf("invalid export recipient")
	}
	bundle, err := json.Marshal(onionKeyBundle{1, addr, base64.StdEncoding.EncodeToString(secret), base64.StdEncoding.EncodeToString(public)})
	if err != nil {
		return nil, err
	}
	sealed, err := box.SealAnonymous(nil, bundle, recipientPub, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("seal onion key: %w", err)
	}
	return sealed, nil
}

// keyDirAddress verifies a service or parked directory's key pair.
func keyDirAddress(dir string) (secret, public []byte, addr string, err error) {
	for _, name := range []string{"hs_ed25519_secret_key", "hs_ed25519_public_key"} {
		st, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !st.Mode().IsRegular() || st.Size() > 256 {
			return nil, nil, "", fmt.Errorf("unsafe stored onion key")
		}
	}
	if secret, err = os.ReadFile(filepath.Join(dir, "hs_ed25519_secret_key")); err != nil {
		return nil, nil, "", err
	}
	if public, err = os.ReadFile(filepath.Join(dir, "hs_ed25519_public_key")); err != nil {
		return nil, nil, "", err
	}
	addr, err = validateOnionKeyPair(secret, public)
	return secret, public, addr, err
}

// A private (restricted-discovery) service must never become public on the
// target: its authorized client list is not part of the transferred bundle.
func hasAuthorizedClients(dir string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "authorized_clients"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

func (t *Tor) transferDir() string { return filepath.Join(t.StateDir, "transfer") }

func (t *Tor) transferPath(deploymentID, cutoverID string) string {
	return filepath.Join(t.transferDir(), deploymentID+"-"+cutoverID+".json")
}

func (t *Tor) readTransferRecipient(deploymentID, cutoverID string) (*onionTransferRecipient, error) {
	path := t.transferPath(deploymentID, cutoverID)
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 1024 || (runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0) {
		return nil, fmt.Errorf("unsafe onion transfer recipient")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var saved onionTransferRecipient
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&saved) != nil || saved.Version != 1 || saved.Deployment != deploymentID ||
		saved.Cutover != cutoverID || !onionAddressRe.MatchString(saved.Onion) || len(saved.Private) != 32 {
		return nil, fmt.Errorf("invalid onion transfer recipient")
	}
	return &saved, nil
}

func x25519Public(private []byte) (*[32]byte, error) {
	key, err := ecdh.X25519().NewPrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("invalid onion transfer recipient")
	}
	var pub [32]byte
	copy(pub[:], key.PublicKey().Bytes())
	return &pub, nil
}

// pruneTransfers deletes recipients older than TransferRetention. Best-effort.
func (t *Tor) pruneTransfers() {
	entries, err := os.ReadDir(t.transferDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-TransferRetention)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(t.transferDir(), e.Name())); err == nil {
			t.Log.Info("proxy/tor: unused onion transfer recipient pruned past retention")
		}
	}
}

// PrepareOnionTransferRecipient creates the recipient for one cutover, or
// returns the one already created for it. It refuses a deployment that
// already publishes an identity: the transferred key must be its only onion.
func (t *Tor) PrepareOnionTransferRecipient(deploymentID, cutoverID, onion string) (*[32]byte, error) {
	if err := validTransferIdentity(deploymentID, cutoverID, onion); err != nil {
		return nil, err
	}
	if err := t.guardRotation(); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(t.StateDir, "services", deploymentID)); err == nil {
		return nil, fmt.Errorf("standby deployment already has an onion identity")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := t.ensureDirs(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(t.transferDir(), 0o700); err != nil {
		return nil, err
	}
	if err := torSafeDirectory(t.transferDir()); err != nil {
		return nil, err
	}
	t.pruneTransfers()
	saved, err := t.readTransferRecipient(deploymentID, cutoverID)
	if err == nil {
		if saved.Onion != onion {
			return nil, fmt.Errorf("onion transfer recipient belongs to another address")
		}
		return x25519Public(saved.Private)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(onionTransferRecipient{1, deploymentID, cutoverID, onion, priv[:]})
	for i := range priv {
		priv[i] = 0
	}
	if err != nil {
		return nil, err
	}
	if err := torPrivateWrite(t.transferPath(deploymentID, cutoverID), raw); err != nil {
		return nil, err
	}
	t.Log.Info("proxy/tor: onion transfer recipient prepared", "deployment_id", deploymentID)
	return pub, nil
}

// WithdrawHiddenService stops publishing a deployment's onion under a failover
// fence. The key set moves to parked/<deployment>-<cutover>, is never deleted
// here, torrc is regenerated without it and a running Tor reloads. Repeating it
// re-verifies the parked identity. A different live identity is refused.
func (t *Tor) WithdrawHiddenService(ctx context.Context, deploymentID, cutoverID, onion string) error {
	if err := validTransferIdentity(deploymentID, cutoverID, onion); err != nil {
		return err
	}
	if err := t.guardRotation(); err != nil {
		return err
	}
	parkedDir := filepath.Join(t.StateDir, "parked")
	parked := filepath.Join(parkedDir, deploymentID+"-"+cutoverID)
	live := filepath.Join(t.StateDir, "services", deploymentID)
	if _, err := os.Lstat(live); err == nil {
		dir, err := t.serviceDir(deploymentID)
		if err != nil {
			return err
		}
		if _, _, addr, err := keyDirAddress(dir); err != nil || addr != onion {
			return fmt.Errorf("live onion identity differs from the reviewed address")
		}
		if _, err := os.Lstat(parked); err == nil {
			return fmt.Errorf("withdrawn onion identity requires recovery")
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(parkedDir, 0o700); err != nil {
			return err
		}
		if err := torSafeDirectory(parkedDir); err != nil {
			return err
		}
		if err := os.Rename(dir, parked); err != nil {
			return fmt.Errorf("withdraw hidden service: %w", err)
		}
		if err := syncRoutingDir(parkedDir); err != nil {
			return err
		}
		if err := syncRoutingDir(filepath.Dir(dir)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := torSafeDirectory(parked); err != nil {
		return fmt.Errorf("withdrawn onion identity cannot be verified")
	} else if _, _, addr, err := keyDirAddress(parked); err != nil || addr != onion {
		return fmt.Errorf("withdrawn onion identity cannot be verified")
	}
	// Purge identifies parked copies by this file; a key set Tor never
	// published may not have one yet.
	if _, err := readOnionStateFile(filepath.Join(parked, "hostname"), 128); os.IsNotExist(err) {
		if err := torPrivateWrite(filepath.Join(parked, "hostname"), []byte(onion+"\n")); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := t.regenerateTorrc(); err != nil {
		return err
	}
	running, err := t.runningForRecovery(ctx)
	if err != nil {
		return err
	}
	if running {
		return t.reloadSecurity(ctx)
	}
	return nil
}

// EnsureWithdrawn is the startup form: a fenced deployment whose live service
// directory is absent publishes nothing, including after its parked copy was
// purged. A live directory is withdrawn again.
func (t *Tor) EnsureWithdrawn(ctx context.Context, deploymentID, cutoverID, onion string) error {
	if err := validTransferIdentity(deploymentID, cutoverID, onion); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(t.StateDir, "services", deploymentID)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return t.WithdrawHiddenService(ctx, deploymentID, cutoverID, onion)
}

// ExportWithdrawnOnionKey seals the parked key set of this cutover to the
// target's recipient. Private services are refused: their client list does
// not travel, and the target would publish them without authorization.
func (t *Tor) ExportWithdrawnOnionKey(deploymentID, cutoverID, onion string, recipientPub *[32]byte) ([]byte, error) {
	if err := validTransferIdentity(deploymentID, cutoverID, onion); err != nil {
		return nil, err
	}
	parked := filepath.Join(t.StateDir, "parked", deploymentID+"-"+cutoverID)
	if err := torSafeDirectory(parked); err != nil {
		return nil, fmt.Errorf("withdrawn onion identity cannot be verified")
	}
	private, err := hasAuthorizedClients(parked)
	if err != nil {
		return nil, err
	}
	if private {
		return nil, fmt.Errorf("a private onion service cannot be transferred")
	}
	secret, public, addr, err := keyDirAddress(parked)
	if err != nil || addr != onion {
		return nil, fmt.Errorf("withdrawn onion identity cannot be verified")
	}
	sealed, err := sealOnionKeyBundle(secret, public, addr, recipientPub)
	if err != nil {
		return nil, err
	}
	t.Log.Info("proxy/tor: withdrawn hidden-service key sealed for the standby", "deployment_id", deploymentID)
	return sealed, nil
}

// ImportTransferredOnionKey opens a bundle sealed to this cutover's recipient
// and installs it as the deployment's identity with the reviewed profile. A
// retry after a committed import verifies the stored identity instead.
func (t *Tor) ImportTransferredOnionKey(deploymentID, cutoverID, onion, sealedB64, profile string) (string, error) {
	if err := validTransferIdentity(deploymentID, cutoverID, onion); err != nil {
		return "", err
	}
	if !validOnionProfile(profile) {
		return "", fmt.Errorf("invalid onion profile")
	}
	if _, err := os.Lstat(filepath.Join(t.StateDir, "services", deploymentID)); err == nil {
		dir, err := t.serviceDir(deploymentID)
		if err != nil {
			return "", err
		}
		if _, _, addr, err := keyDirAddress(dir); err != nil || addr != onion {
			return "", fmt.Errorf("deployment already has a different onion identity")
		}
		current, err := t.readProfile(deploymentID)
		if err != nil || string(current) != profile {
			return "", fmt.Errorf("stored onion profile differs from the transfer")
		}
		t.forgetTransferRecipient(deploymentID, cutoverID)
		return onion, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	saved, err := t.readTransferRecipient(deploymentID, cutoverID)
	if err != nil {
		return "", fmt.Errorf("onion transfer recipient is unavailable")
	}
	if saved.Onion != onion {
		return "", fmt.Errorf("onion transfer recipient belongs to another address")
	}
	sealed, err := base64.StdEncoding.DecodeString(sealedB64)
	if err != nil || len(sealed) <= box.AnonymousOverhead || len(sealed) > 8192 ||
		base64.StdEncoding.EncodeToString(sealed) != sealedB64 {
		return "", fmt.Errorf("invalid sealed onion identity")
	}
	pub, err := x25519Public(saved.Private)
	if err != nil {
		return "", err
	}
	var priv [32]byte
	copy(priv[:], saved.Private)
	raw, ok := box.OpenAnonymous(nil, sealed, pub, &priv)
	for i := range priv {
		priv[i] = 0
	}
	if !ok {
		return "", fmt.Errorf("sealed onion identity was not created for this recipient")
	}
	var bundle onionKeyBundle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&bundle) != nil || bundle.Version != 1 || bundle.Onion != onion {
		return "", fmt.Errorf("sealed onion identity does not match the reviewed address")
	}
	secret, err := base64.StdEncoding.DecodeString(bundle.Secret)
	if err != nil {
		return "", fmt.Errorf("invalid transferred onion key")
	}
	public, err := base64.StdEncoding.DecodeString(bundle.Public)
	if err != nil {
		return "", fmt.Errorf("invalid transferred onion key")
	}
	// Verify before installing: a wrong identity must never reach services/.
	if addr, err := validateOnionKeyPair(secret, public); err != nil || addr != onion {
		return "", fmt.Errorf("transferred onion key does not match the reviewed address")
	}
	addr, err := t.ImportOnionKeyWithProfile(deploymentID, secret, public, profile)
	if err != nil {
		return "", err
	}
	t.forgetTransferRecipient(deploymentID, cutoverID)
	return addr, nil
}

func (t *Tor) forgetTransferRecipient(deploymentID, cutoverID string) {
	if err := os.Remove(t.transferPath(deploymentID, cutoverID)); err != nil && !os.IsNotExist(err) {
		t.Log.Warn("proxy/tor: onion transfer recipient retained after import", "deployment_id", deploymentID)
		return
	}
	if err := syncRoutingDir(t.transferDir()); err != nil {
		t.Log.Warn("proxy/tor: onion transfer recipient removal not synced", "deployment_id", deploymentID)
	}
}
