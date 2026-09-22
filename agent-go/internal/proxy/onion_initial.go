package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func writeInitialOnionProfile(dir, profile string) error {
	data, err := json.Marshal(map[string]string{"profile": profile})
	if err != nil {
		return err
	}
	return torPrivateWrite(filepath.Join(dir, "profile.json"), append(data, '\n'))
}

// Validate max against the image before installing any service directory. This
// also works on a cold host, where there is no running daemon to interrogate.
func (t *Tor) CheckInitialProfile(ctx context.Context, profile string) error {
	if !validOnionProfile(profile) {
		return fmt.Errorf("invalid initial onion profile")
	}
	if profile != "max" {
		return nil
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(check, "docker", "run", "--rm", "--network=none", "--memory=128m", TorImage, "tor", "--list-modules").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "pow: yes") {
		return fmt.Errorf("initial max profile requires the Tor PoW module")
	}
	return nil
}

// PrepareInitialProfile installs metadata before any torrc can mention the
// service. Retries may reuse the same tier, but cannot silently alter an
// existing service: later changes use the journaled profile operation.
func (t *Tor) PrepareInitialProfile(deploymentID, profile string) error {
	if !onionDeploymentID.MatchString(deploymentID) || !validOnionProfile(profile) {
		return fmt.Errorf("invalid initial onion profile context")
	}
	if err := t.guardRotation(); err != nil {
		return err
	}
	if err := t.ensureDirs(); err != nil {
		return err
	}
	dir := filepath.Join(t.StateDir, "services", deploymentID)
	if _, err := os.Lstat(dir); err == nil {
		current, err := t.readProfile(deploymentID)
		if err != nil {
			return err
		}
		if string(current) != profile {
			return fmt.Errorf("existing onion profile differs; use the explicit profile update")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	staging, err := os.MkdirTemp(t.StateDir, "initial-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := writeInitialOnionProfile(staging, profile); err != nil {
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	return syncRoutingDir(filepath.Dir(dir))
}
