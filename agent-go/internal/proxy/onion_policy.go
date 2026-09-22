package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The daemon can reload independently after a host restart. Keep a durable
// before-image for every policy mutation and recover it before accepting work.
type onionPolicyChange struct {
	Version    int               `json:"version"`
	Deployment string            `json:"deployment"`
	Kind       string            `json:"kind"`
	Auth       map[string][]byte `json:"auth,omitempty"`
	Profile    []byte            `json:"profile,omitempty"`
	HadProfile bool              `json:"had_profile"`
	Committed  bool              `json:"committed"`
	Resume     bool              `json:"resume,omitempty"`
}

func (t *Tor) policyPath() string { return filepath.Join(t.StateDir, "policy-change.json") }

func (t *Tor) beginPolicyChange(change onionPolicyChange) error {
	if err := t.guardRotation(); err != nil {
		return err
	}
	change.Version = 1
	data, err := json.Marshal(change)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(t.policyPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("policy journal requires recovery: %w", err)
	}
	return syncRoutingDir(t.StateDir)
}

// Called by a deferred closure, including write, removal, render and reload errors.
func (t *Tor) finishPolicyChange(ctx context.Context, result *error) {
	if *result == nil {
		change, err := t.readPolicyChange()
		if err == nil {
			change.Committed = true
			var data []byte
			data, err = json.Marshal(change)
			if err == nil {
				err = torPrivateWrite(t.policyPath(), data)
			}
		}
		if err == nil {
			if err = t.clearPolicyChange(); err != nil {
				t.Log.Warn("onion policy committed; journal cleanup requires recovery", "err", err)
			}
			return
		}
		*result = fmt.Errorf("policy commit requires recovery: %w", err)
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
	defer cancel()
	if err := t.RecoverOnionPolicy(recovery); err != nil {
		*result = fmt.Errorf("policy change failed; recovery required: %w", err)
	}
}

func (t *Tor) clearPolicyChange() error {
	if err := os.Remove(t.policyPath()); err != nil {
		return err
	}
	return syncRoutingDir(t.StateDir)
}

func (t *Tor) readPolicyChange() (onionPolicyChange, error) {
	var change onionPolicyChange
	st, err := os.Lstat(t.policyPath())
	if err != nil {
		return change, err
	}
	if !st.Mode().IsRegular() || st.Size() > 256*1024 {
		return change, fmt.Errorf("unsafe onion policy journal")
	}
	data, err := os.ReadFile(t.policyPath())
	if err != nil {
		return change, err
	}
	if json.Unmarshal(data, &change) != nil || change.Version != 1 || !onionDeploymentID.MatchString(change.Deployment) || (change.Kind != "auth" && change.Kind != "profile") {
		return change, fmt.Errorf("invalid onion policy journal")
	}
	for name, data := range change.Auth {
		base, ok := strings.CutSuffix(name, ".auth")
		pub, hasPrefix := strings.CutPrefix(strings.TrimSpace(string(data)), "descriptor:x25519:")
		if !ok || !onionClientNameRe.MatchString(base) || !hasPrefix || !validOnionClientKey(pub) {
			return change, fmt.Errorf("invalid authorization recovery state")
		}
	}
	if change.HadProfile {
		var p struct {
			Profile string `json:"profile"`
		}
		if json.Unmarshal(change.Profile, &p) != nil || !validOnionProfile(p.Profile) {
			return change, fmt.Errorf("invalid profile recovery state")
		}
	}
	return change, nil
}

func (t *Tor) RecoverOnionPolicy(ctx context.Context) error {
	change, err := t.readPolicyChange()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if change.Committed {
		return t.clearPolicyChange()
	}
	dir, err := t.serviceDir(change.Deployment)
	if err != nil {
		return err
	}
	if change.Kind == "auth" {
		// Recovery can itself be interrupted. Disable Docker autostart before
		// restoring files, just as in the forward transaction.
		if err := t.pauseSecurity(ctx); err != nil {
			return err
		}
		auth := filepath.Join(dir, "authorized_clients")
		if err := os.MkdirAll(auth, 0700); err != nil {
			return err
		}
		if err := torSafeDirectory(auth); err != nil {
			return err
		}
		// Restore old keys before removing additions, keeping private services
		// private even if a subsequent disk write or removal fails.
		for name, data := range change.Auth {
			if err := torPrivateWrite(filepath.Join(auth, name), data); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(auth)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".auth") {
				continue
			}
			if _, keep := change.Auth[e.Name()]; keep {
				continue
			}
			if !e.Type().IsRegular() {
				return fmt.Errorf("unsafe authorization recovery entry")
			}
			if err := os.Remove(filepath.Join(auth, e.Name())); err != nil {
				return err
			}
		}
		if err := syncRoutingDir(auth); err != nil {
			return err
		}
	} else {
		path := filepath.Join(dir, "profile.json")
		if change.HadProfile {
			if err := torPrivateWrite(path, change.Profile); err != nil {
				return err
			}
		} else {
			if st, err := os.Lstat(path); err == nil {
				if !st.Mode().IsRegular() {
					return fmt.Errorf("unsafe profile recovery entry")
				}
				if err := os.Remove(path); err != nil {
					return err
				}
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := syncRoutingDir(dir); err != nil {
				return err
			}
		}
		if err := t.regenerateTorrc(); err != nil {
			return err
		}
	}
	running, err := t.runningForRecovery(ctx)
	if err != nil {
		return err
	}
	if running || (change.Kind == "auth" && change.Resume) {
		apply := t.reloadSecurity
		if change.Kind == "auth" {
			apply = t.restartSecurity
		}
		if err := apply(ctx); err != nil {
			return err
		}
	}
	return t.clearPolicyChange()
}
