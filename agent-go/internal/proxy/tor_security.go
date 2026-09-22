package proxy

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func validOnionClientKey(value string) bool {
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
	if err != nil || len(raw) != 32 || base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw) != strings.ToUpper(value) {
		return false
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return false
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return false
	}
	_, err = key.ECDH(pub)
	return err == nil
}

func torPrivateWrite(path string, data []byte) error {
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular Tor state")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncRoutingDir(filepath.Dir(path))
}

func torSafeDirectory(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe Tor directory")
	}
	return nil
}

func (t *Tor) reloadSecurity(ctx context.Context) error {
	if t.reloadFn != nil {
		return t.reloadFn(ctx)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(checkCtx, "docker", "exec", TorContainer, "tor", "--verify-config", "-f", "/etc/tor/torrc").Run(); err != nil {
		return fmt.Errorf("Tor configuration validation failed: %w", err)
	}
	if err := exec.CommandContext(checkCtx, "docker", "kill", "-s", "HUP", TorContainer).Run(); err != nil {
		return fmt.Errorf("Tor did not accept configuration reload: %w", err)
	}
	if !t.isRunning(checkCtx) {
		return fmt.Errorf("Tor is not running after configuration reload")
	}
	return nil
}

func (t *Tor) Reload(ctx context.Context) error { return t.reloadSecurity(ctx) }

// Keep an interrupted authorization write offline across Docker/host restarts.
// The policy journal is durable BEFORE this function is called, and contains
// whether recovery must resume the daemon. No files may change until stop wins.
func (t *Tor) pauseSecurity(ctx context.Context) error {
	if t.reloadFn != nil {
		return nil
	}
	running, err := t.runningForRecovery(ctx)
	if err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "docker", "update", "--restart=no", TorContainer).Run(); err != nil {
		return fmt.Errorf("cannot suspend Tor automatic restart: %w", err)
	}
	if running {
		if err := exec.CommandContext(ctx, "docker", "stop", "--time", "10", TorContainer).Run(); err != nil {
			return fmt.Errorf("cannot stop Tor before authorization change: %w", err)
		}
	}
	return nil
}

// Authorization revocation must discard existing rendezvous state, not merely
// reread the files. Tor's client-authorization contract requires a restart.
// The shared daemon briefly interrupts onion connections on this host.
func (t *Tor) restartSecurity(ctx context.Context) error {
	if t.reloadFn != nil {
		return t.reloadFn(ctx)
	}
	deadline, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := exec.CommandContext(deadline, "docker", "restart", "--time", "10", TorContainer).Run(); err != nil {
		return fmt.Errorf("Tor authorization restart failed: %w", err)
	}
	if err := t.waitReady(deadline); err != nil {
		return err
	}
	if err := exec.CommandContext(deadline, "docker", "exec", TorContainer, "tor", "--verify-config", "-f", "/etc/tor/torrc").Run(); err != nil {
		return fmt.Errorf("Tor authorization configuration validation failed: %w", err)
	}
	if err := exec.CommandContext(deadline, "docker", "update", "--restart=unless-stopped", TorContainer).Run(); err != nil {
		return fmt.Errorf("cannot restore Tor automatic restart: %w", err)
	}
	return nil
}

// Recovery must distinguish a stopped daemon from an unreachable Docker API.
// Treating both as "not running" could discard the only recovery record while
// a live daemon still serves a partially applied policy.
func (t *Tor) runningForRecovery(ctx context.Context) (bool, error) {
	if t.reloadFn != nil {
		return true, nil
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Status}}", TorContainer).CombinedOutput()
	if err != nil {
		if dockerObjectMissing(out) {
			return false, nil
		}
		return false, fmt.Errorf("cannot verify Tor recovery state: %w", err)
	}
	switch strings.TrimSpace(string(out)) {
	case "running":
		return true, nil
	case "created", "exited", "dead":
		return false, nil
	default:
		return false, fmt.Errorf("Tor recovery waits for a stable daemon state")
	}
}

func (t *Tor) serviceDir(id string) (string, error) {
	if !onionDeploymentID.MatchString(id) {
		return "", fmt.Errorf("invalid deployment ID")
	}
	dir := filepath.Join(t.StateDir, "services", id)
	for _, p := range []string{t.StateDir, filepath.Dir(dir), dir} {
		st, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unsafe Tor service directory")
		}
	}
	return dir, nil
}
