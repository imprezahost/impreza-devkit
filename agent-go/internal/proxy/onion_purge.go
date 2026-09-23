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
// control plane; here the agent deletes only parked directories whose
// hostname file matches it — never the live identity, never another
// deployment's material, never a directory it cannot identify.
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
	parkedDir := filepath.Join(t.StateDir, "parked")
	if err := torSafeDirectory(parkedDir); err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(parkedDir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := torSafeDirectory(parkedDir); err != nil {
		return nil, err
	}
	purged := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name != deploymentID && !strings.HasPrefix(name, deploymentID+"-") {
			continue
		}
		dir := filepath.Join(parkedDir, name)
		raw, err := readOnionStateFile(filepath.Join(dir, "hostname"), 128)
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot identify a retained directory; purge is not verified")
		}
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(strings.TrimSpace(string(raw)), address) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("purge parked identity: %w", err)
		}
		purged = append(purged, name)
	}
	if len(purged) > 0 {
		if err := syncRoutingDir(parkedDir); err != nil {
			return purged, err
		}
		t.Log.Info("proxy/tor: parked identity purged after customer confirmation",
			"deployment_id", deploymentID, "copies", len(purged))
	} else {
		t.Log.Info("proxy/tor: purge found no retained copy of the address",
			"deployment_id", deploymentID)
	}
	return purged, nil
}
