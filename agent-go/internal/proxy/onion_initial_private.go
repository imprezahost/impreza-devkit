package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareInitialPrivate publishes the directory atomically, including its
// authorization policy. No torrc scan can ever see this service as public.
// A replay verifies the original request and preserves subsequent revocations.
func (t *Tor) PrepareInitialPrivate(id, profile string, clients map[string]string) error {
	if !onionDeploymentID.MatchString(id) || !validOnionProfile(profile) || len(clients) == 0 || len(clients) > 16 {
		return fmt.Errorf("invalid initial private onion policy")
	}
	for name, key := range clients {
		if !onionClientNameRe.MatchString(name) || !validOnionClientKey(key) {
			return fmt.Errorf("invalid initial onion client")
		}
	}
	if err := t.guardRotation(); err != nil {
		return err
	}
	if err := t.ensureDirs(); err != nil {
		return err
	}
	data, _ := json.Marshal(struct {
		Profile string
		Clients map[string]string
	}{profile, clients})
	digest := sha256.Sum256(data)
	marker := hex.EncodeToString(digest[:])
	dir := filepath.Join(t.StateDir, "services", id)
	if _, err := os.Lstat(dir); err == nil {
		if err := torSafeDirectory(dir); err != nil {
			return err
		}
		raw, err := readOnionStateFile(filepath.Join(dir, "initial-private.sha256"), 128)
		if err != nil || string(raw) != marker {
			return fmt.Errorf("existing onion was not initialized with this private policy")
		}
		// An explicit later transition to public must not satisfy a private retry.
		names, err := t.OnionClientNames(id)
		if err != nil || len(names) == 0 {
			return fmt.Errorf("existing onion is no longer private")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	staging, err := os.MkdirTemp(t.StateDir, "initial-private-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := writeInitialOnionProfile(staging, profile); err != nil {
		return err
	}
	auth := filepath.Join(staging, "authorized_clients")
	if err := os.Mkdir(auth, 0700); err != nil {
		return err
	}
	if err := applyOnionClients(auth, clients, nil); err != nil {
		return err
	}
	if err := torPrivateWrite(filepath.Join(staging, "initial-private.sha256"), []byte(marker)); err != nil {
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	return syncRoutingDir(filepath.Dir(dir))
}

// State files must be bounded regular files; do not follow filesystem links.
func readOnionStateFile(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("unsafe onion state file")
	}
	return os.ReadFile(path)
}
