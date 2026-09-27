package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var onionAddressRe = regexp.MustCompile(`^([a-z2-7]{56})\.onion$`)

// PurgeOnionIdentity deletes the managed parked recovery copies of ONE
// address of one deployment. The customer confirmed the exact address on the
// control plane; here the agent deletes only retained directories it
// identifies as that address (see retainedAddress) — never the live
// identity, never another deployment's material, never a directory it
// cannot identify. Every copy is identified before any is deleted, so a
// directory that cannot be identified fails the purge without a partial
// delete.
//
// Rotation receipts are kept: they carry no key material and protect
// rotation idempotency after crashes. Uninstall-parked copies of the same
// address are purged together — retention exists for accidents, and an
// explicit customer-confirmed purge is not an accident.
//
// A matching address that turns out to have no parked copy is a success
// with zero copies. This covers the managed retention directories, not
// external exports, backups or forensic recovery of deleted filesystem data.
func (t *Tor) PurgeOnionIdentity(ctx context.Context, deploymentID, address string) ([]string, error) {
	if !onionDeploymentID.MatchString(deploymentID) {
		return nil, fmt.Errorf("invalid deployment ID")
	}
	address = strings.ToLower(strings.TrimSpace(address))
	if !onionAddressRe.MatchString(address) {
		return nil, fmt.Errorf("invalid onion address")
	}
	if err := t.guardRotation(); err != nil {
		return nil, err
	}
	// The live identity is never purge material: rotation or uninstall moves
	// an identity to parked/ before it becomes destroyable. Refuse while the
	// address still serves.
	if svcDir, err := t.serviceDir(deploymentID); err == nil {
		raw, err := readOnionStateFile(filepath.Join(svcDir, "hostname"), 128)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && strings.EqualFold(strings.TrimSpace(string(raw)), address) {
			return nil, fmt.Errorf("address is the active identity of this deployment")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// Quarantined material joins the purge too: a quarantined directory can
	// still hold key material of this address, and a customer-confirmed
	// purge must not leave it behind.
	type retainedCopy struct{ parent, name string }
	var matches []retainedCopy
	quarantineDir := filepath.Join(t.StateDir, "quarantine")
	for _, parent := range []string{filepath.Join(t.StateDir, "parked"), quarantineDir} {
		if err := torSafeDirectory(parent); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if name != deploymentID && !strings.HasPrefix(name, deploymentID+"-") {
				continue
			}
			addr, err := retainedAddress(filepath.Join(parent, name), parent == quarantineDir)
			if err != nil {
				return nil, err
			}
			if addr != "" && strings.EqualFold(addr, address) {
				matches = append(matches, retainedCopy{parent, name})
			}
		}
	}
	purged := []string{}
	touched := map[string]bool{}
	for _, m := range matches {
		if err := os.RemoveAll(filepath.Join(m.parent, m.name)); err != nil {
			return purged, fmt.Errorf("purge parked identity: %w", err)
		}
		purged = append(purged, m.name)
		touched[m.parent] = true
	}
	for parent := range touched {
		if err := syncRoutingDir(parent); err != nil {
			return purged, err
		}
	}
	if len(purged) > 0 {
		t.Log.Info("proxy/tor: parked identity purged after customer confirmation",
			"deployment_id", deploymentID, "copies", len(purged))
	} else {
		t.Log.Info("proxy/tor: purge found no retained copy of the address",
			"deployment_id", deploymentID)
	}
	return purged, nil
}

// retainedAddress reports the onion address a parked or quarantined
// directory holds material of, or "" when it holds no identity at all.
//
// The key pair is authoritative: the hostname file is written inside
// services/, which the Tor container can write, and a key set quarantined
// before Tor ever published it has no hostname — the same case as a parked
// staging. Without a verifiable pair the hostname identifies the copy.
// Key material that cannot be attributed to an address fails the purge.
//
// With neither key material nor hostname there is no identity to destroy:
// a quarantined entry (the agent already judged its content unusable) and
// a parked staging an older agent left with only its profile are
// left to retention. Anything else in parked/, which only the agent
// writes, is unexpected and fails the purge.
func retainedAddress(dir string, quarantined bool) (string, error) {
	_, keyErr := os.Lstat(filepath.Join(dir, "hs_ed25519_secret_key"))
	if keyErr != nil && !os.IsNotExist(keyErr) {
		return "", keyErr
	}
	hasKey := keyErr == nil
	if hasKey {
		if _, _, addr, err := keyDirAddress(dir); err == nil {
			return addr, nil
		}
	}
	raw, err := readOnionStateFile(filepath.Join(dir, "hostname"), 128)
	if err == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	unverified := fmt.Errorf("cannot identify a retained directory; purge is not verified")
	if hasKey {
		return "", unverified
	}
	if quarantined {
		return "", nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() != "profile.json" && e.Name() != "authorized_clients" {
			return "", unverified
		}
	}
	return "", nil
}
