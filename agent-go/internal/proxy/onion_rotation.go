package proxy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var onionCommandID = regexp.MustCompile(`^cmd_[a-zA-Z0-9_-]{1,60}$`)

type onionRotation struct {
	Version    int    `json:"version"`
	Deployment string `json:"deployment"`
	Command    string `json:"command"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Secret     []byte `json:"secret"`
	Public     []byte `json:"public"`
	Fragment   []byte `json:"fragment"`
}

func (t *Tor) rotationPath() string { return filepath.Join(t.StateDir, "rotation.json") }
func (t *Tor) guardRotation() error {
	if _, err := os.Lstat(t.policyPath()); !os.IsNotExist(err) {
		return fmt.Errorf("onion policy recovery required before changing hidden services")
	}
	if _, err := os.Lstat(t.rotationPath()); os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("onion rotation recovery required before changing hidden services")
}

func onionReplacement(raw, previous, next string) (string, error) {
	header, blocks, err := splitFragment(raw)
	if err != nil {
		return "", err
	}
	count := 0
	for i, block := range blocks {
		lines := strings.Split(block, "\n")
		if lines[0] == "http://"+previous+" {" {
			lines[0] = "http://" + next + " {"
			count++
		}
		for j, line := range lines {
			if strings.TrimSpace(line) == "header Onion-Location \"http://"+previous+"/\"" {
				lines[j] = "  header Onion-Location \"http://" + next + "/\""
			}
		}
		blocks[i] = strings.Join(lines, "\n")
	}
	if count != 1 {
		return "", fmt.Errorf("expected exactly one current onion route")
	}
	return secureOnionFragment(strings.Join(append(header, blocks...), "\n") + "\n")
}

func freshOnionKeyFiles() ([]byte, []byte, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	expanded := sha512.Sum512(priv.Seed())
	expanded[0] &= 248
	expanded[31] &= 127
	expanded[31] |= 64
	secret := append(append([]byte(secretKeyHeader), 0, 0, 0), expanded[:]...)
	public := append(append([]byte(publicKeyHeader), 0, 0, 0), pub...)
	address, err := validateOnionKeyPair(secret, public)
	return secret, public, address, err
}

// RotateOnionIdentity changes only the two identity files and hostname. Access
// policy stays in place. A durable before-image covers keys AND Caddy routing;
// startup rolls back unfinished work before advertising a service again.
func (t *Tor) RotateOnionIdentity(ctx context.Context, c *Caddy, deployment, command, expected string) (address string, finalErr error) {
	if c == nil || !onionCommandID.MatchString(command) {
		return "", fmt.Errorf("invalid rotation context")
	}
	dir, err := t.serviceDir(deployment)
	if err != nil {
		return "", err
	}
	if err = t.guardRotation(); err != nil {
		return "", err
	}
	if err = c.guardRoutingSwitch(); err != nil {
		return "", err
	}
	receipt := filepath.Join(t.StateDir, "rotation-receipts", command+".json")
	if raw, err := os.ReadFile(receipt); err == nil {
		var saved struct{ Deployment, Before, After string }
		if json.Unmarshal(raw, &saved) != nil || saved.Deployment != deployment || saved.Before != expected {
			return "", fmt.Errorf("rotation receipt does not match request")
		}
		return saved.After, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	secret, err := os.ReadFile(filepath.Join(dir, "hs_ed25519_secret_key"))
	if err != nil {
		return "", err
	}
	public, err := os.ReadFile(filepath.Join(dir, "hs_ed25519_public_key"))
	if err != nil {
		return "", err
	}
	before, err := validateOnionKeyPair(secret, public)
	if err != nil {
		return "", err
	}
	if before != expected {
		return "", fmt.Errorf("onion identity changed since confirmation")
	}
	freshSecret, freshPublic, after, err := freshOnionKeyFiles()
	if err != nil {
		return "", err
	}
	fragmentPath := filepath.Join(c.StateDir, "deployments", deployment+".caddy")
	fragment, err := os.ReadFile(fragmentPath)
	if err != nil {
		return "", err
	}
	replacement, err := onionReplacement(string(fragment), before, after)
	if err != nil {
		return "", err
	}
	record := onionRotation{1, deployment, command, before, after, secret, public, fragment}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	journal, err := os.OpenFile(t.rotationPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, err = journal.Write(raw)
	if err == nil {
		err = journal.Sync()
	}
	closeErr := journal.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncRoutingDir(t.StateDir)
	}
	if err != nil {
		return "", err
	}
	applyErr := func() error {
		for _, file := range []struct {
			name string
			data []byte
		}{{"hs_ed25519_secret_key", freshSecret}, {"hs_ed25519_public_key", freshPublic}, {"hostname", []byte(after + "\n")}} {
			if err := torPrivateWrite(filepath.Join(dir, file.name), file.data); err != nil {
				return err
			}
		}
		if err := writeSwitchFile(fragmentPath, []byte(replacement)); err != nil {
			return err
		}
		if err := c.regenerateCaddyfile(); err != nil {
			return err
		}
		if err := c.reloadSwitch(ctx); err != nil {
			return err
		}
		return t.reloadSecurity(ctx)
	}()
	if applyErr != nil {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := t.RecoverOnionRotation(recovery, c); err != nil {
			return "", fmt.Errorf("rotation failed; recovery required: %w", err)
		}
		return "", applyErr
	}
	// Failures while persisting the backup or receipt also restore both layers.
	defer func() {
		if finalErr == nil {
			return
		}
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := t.RecoverOnionRotation(recovery, c); err != nil {
			finalErr = fmt.Errorf("rotation persistence failed; recovery required: %w", err)
		}
	}()
	// Park just the prior identity under a unique command directory. Policies are
	// never moved out of the live service, even transiently.
	backup := filepath.Join(t.StateDir, "parked", deployment+"-"+command)
	if err := os.MkdirAll(backup, 0700); err != nil {
		return "", err
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"hs_ed25519_secret_key", secret}, {"hs_ed25519_public_key", public}, {"hostname", []byte(before + "\n")}} {
		if err := torPrivateWrite(filepath.Join(backup, file.name), file.data); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(receipt), 0700); err != nil {
		return "", err
	}
	saved, _ := json.Marshal(struct{ Deployment, Before, After string }{deployment, before, after})
	if err := torPrivateWrite(receipt, saved); err != nil {
		return "", err
	}
	if err := os.Remove(t.rotationPath()); err != nil {
		t.Log.Warn("committed rotation journal retained for startup reconciliation", "deployment_id", deployment)
		return after, nil
	}
	if err := syncRoutingDir(t.StateDir); err != nil {
		t.Log.Warn("committed rotation cleanup requires startup reconciliation", "deployment_id", deployment)
	}
	return after, nil
}

// RecoverOnionRotation refuses unknown/corrupt journals. It never generates a
// key or retries a destructive operation after a crash.
func (t *Tor) RecoverOnionRotation(ctx context.Context, c *Caddy) error {
	st, err := os.Lstat(t.rotationPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > 4*1024*1024 {
		return fmt.Errorf("unsafe rotation journal")
	}
	raw, err := os.ReadFile(t.rotationPath())
	if err != nil {
		return err
	}
	var record onionRotation
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 || !onionCommandID.MatchString(record.Command) {
		return fmt.Errorf("invalid rotation journal")
	}
	dir, err := t.serviceDir(record.Deployment)
	if err != nil {
		return err
	}
	address, err := validateOnionKeyPair(record.Secret, record.Public)
	if err != nil || address != record.Before {
		return fmt.Errorf("invalid recovery identity")
	}
	if c == nil {
		return fmt.Errorf("proxy required for onion recovery")
	}
	if receipt, err := os.ReadFile(filepath.Join(t.StateDir, "rotation-receipts", record.Command+".json")); err == nil {
		var saved struct{ Deployment, Before, After string }
		if json.Unmarshal(receipt, &saved) != nil || saved.Deployment != record.Deployment || saved.Before != record.Before || saved.After != record.After {
			return fmt.Errorf("invalid rotation receipt")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		for _, file := range []struct {
			name string
			data []byte
		}{{"hs_ed25519_secret_key", record.Secret}, {"hs_ed25519_public_key", record.Public}, {"hostname", []byte(record.Before + "\n")}} {
			if err := torPrivateWrite(filepath.Join(dir, file.name), file.data); err != nil {
				return err
			}
		}
		if err := writeSwitchFile(filepath.Join(c.StateDir, "deployments", record.Deployment+".caddy"), record.Fragment); err != nil {
			return err
		}
		if err := c.regenerateCaddyfile(); err != nil {
			return err
		}
		if err := c.reloadSwitch(ctx); err != nil {
			return err
		}
		running, err := t.runningForRecovery(ctx)
		if err != nil {
			return err
		}
		if running {
			if err := t.reloadSecurity(ctx); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(t.rotationPath()); err != nil {
		return err
	}
	return syncRoutingDir(t.StateDir)
}
