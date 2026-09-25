package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"gopkg.in/yaml.v3"
)

// Agent sandbox runtime class (capability agent-sandbox-v1).
//
// A sandbox deployment is structurally ephemeral and confined: every
// capability dropped, no-new-privileges, read-only root filesystem with a
// tmpfs /tmp, no host binds (the Docker socket above all), no host NAT
// ports (ingress only via the managed proxy), no restart policy, and a
// hard wall-clock lifetime enforced by the agent across restarts. Docker's
// default seccomp profile remains applied; additional restrictive
// seccomp/AppArmor layers are a registered pendência, and this class is
// abuse containment — NOT a universal isolation guarantee.

// SandboxSpec is the manifest's runtime.sandbox block, shared with the SDK.
type SandboxSpec = sdkclient.SandboxSpec

const SandboxProtocol = sdkclient.SandboxProtocol

const sandboxDeadlineFile = "sandbox-deadline.json"

// applySandbox structurally rewrites the effective Compose (never by
// concatenating YAML), in the applyTorEgress mold.
func applySandbox(composeYAML, deploymentID string, spec SandboxSpec) (string, error) {
	if len(composeYAML) > 2<<20 || !recoveryDeploymentID.MatchString(deploymentID) {
		return "", errors.New("invalid sandbox deployment")
	}
	if spec.MaxLifetimeMinutes < 5 || spec.MaxLifetimeMinutes > 1440 {
		return "", errors.New("sandbox max_lifetime_minutes must be 5-1440")
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(composeYAML), &node); err != nil {
		return "", errors.New("invalid sandbox Compose")
	}
	count := 0
	var check func(*yaml.Node) error
	check = func(n *yaml.Node) error {
		count++
		if count > 20000 || n.Kind == yaml.AliasNode || n.Anchor != "" {
			return errors.New("sandbox Compose does not support aliases or oversized documents")
		}
		for _, child := range n.Content {
			if err := check(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(&node); err != nil {
		return "", err
	}
	var doc map[string]any
	if err := node.Decode(&doc); err != nil {
		return "", errors.New("invalid sandbox Compose mapping")
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 16 {
		return "", errors.New("sandbox requires 1-16 services")
	}
	if _, exists := doc["include"]; exists {
		return "", errors.New("sandbox refuses Compose includes")
	}
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			return "", errors.New("invalid sandbox service")
		}
		for _, key := range []string{"privileged", "cap_add", "devices", "device_cgroup_rules", "pid", "ipc", "network_mode", "extends", "provider"} {
			if _, exists := service[key]; exists {
				return "", fmt.Errorf("sandbox refuses service option %s", key)
			}
		}
		// No persistent storage of any kind — host binds, named volumes,
		// and above all the Docker socket. Ephemeral means tmpfs only, so a
		// declared volume is a manifest error, not something to silently drop.
		for _, key := range []string{"volumes", "binds"} {
			if items, exists := service[key].([]any); exists && len(items) > 0 {
				return "", errors.New("sandbox refuses declared volumes (ephemeral runtime: tmpfs only)")
			}
		}
	// Drop everything, then restore ONLY the classic daemon set — without
	// CHOWN/SETUID/SETGID/DAC_OVERRIDE even stock images like nginx:alpine
	// die at startup (chown on temp dirs, worker uid drop). The dangerous
	// capabilities (SYS_ADMIN, NET_ADMIN, SYS_PTRACE, ...) stay dropped and
	// no-new-privileges remains.
	service["cap_drop"] = []string{"ALL"}
	service["cap_add"] = []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"}
		service["security_opt"] = []string{"no-new-privileges:true"}
		service["read_only"] = true
		// Read-only rootfs still needs the conventional writable paths:
		// /tmp and the runtime dir (/run; /var/run symlinks to it). Apps
		// with other writable locations declare them in ExtraTmpfs — the
		// mountpoint itself always exists, so non-recursive mkdirs under
		// it (nginx temp dirs) keep working.
		tmpfs := []string{
			"/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777",
			"/run:rw,nosuid,nodev,noexec,size=16m",
		}
		for _, extra := range spec.ExtraTmpfs {
			if !strings.HasPrefix(extra, "/") || strings.Contains(extra, "..") || len(extra) > 128 {
				return "", fmt.Errorf("sandbox extra_tmpfs path %q is not a safe absolute path", extra)
			}
			if strings.Contains(extra, ":") {
				return "", fmt.Errorf("sandbox extra_tmpfs path %q must not carry mount options", extra)
			}
			tmpfs = append(tmpfs, extra+":rw,nosuid,nodev,noexec,size=64m")
		}
		if len(tmpfs) > 6 {
			return "", errors.New("sandbox supports at most 4 extra tmpfs paths")
		}
		service["tmpfs"] = tmpfs
		service["restart"] = "no" // ephemeral by contract
		delete(service, "ports")  // ingress only via the managed reverse proxy
		services[name] = service
	}
	// Top-level named volumes are meaningless once binds are gone.
	delete(doc, "volumes")
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return "", errors.New("cannot encode sandbox Compose")
	}
	return string(raw), nil
}

// writeSandboxDeadline records the wall-clock budget next to the app's
// compose so a restarted agent still enforces it.
func writeSandboxDeadline(appDir string, spec SandboxSpec) error {
	payload := map[string]any{
		"deadline_unix": time.Now().Add(time.Duration(spec.MaxLifetimeMinutes) * time.Minute).Unix(),
		"version":       1,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(appDir, sandboxDeadlineFile), raw, 0o600)
}

// clearSandboxDeadline removes the marker (uninstall / class removed).
func clearSandboxDeadline(appDir string) {
	_ = os.Remove(filepath.Join(appDir, sandboxDeadlineFile))
}

// ReconcileSandbox stops deployments whose wall-clock budget expired. The
// poller calls it between commands; it is a cheap directory scan that does
// nothing without markers. Containers are STOPPED (not removed): the
// customer's data dir and images stay, the deployment simply refuses to
// keep burning compute past its budget.
func (d *Docker) ReconcileSandbox(ctx context.Context) {
	entries, err := os.ReadDir(filepath.Join(d.StateDir, "apps"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !runtimeDeploymentID.MatchString(entry.Name()) {
			continue
		}
		appDir := filepath.Join(d.StateDir, "apps", entry.Name())
		raw, err := os.ReadFile(filepath.Join(appDir, sandboxDeadlineFile))
		if err != nil {
			continue
		}
		var marker struct {
			DeadlineUnix int64 `json:"deadline_unix"`
		}
		if json.Unmarshal(raw, &marker) != nil || marker.DeadlineUnix == 0 {
			continue
		}
		if time.Now().Unix() < marker.DeadlineUnix {
			continue
		}
		d.Log.Info("sandbox: wall-clock budget expired, stopping deployment", "deployment_id", entry.Name())
		stopCtx, cancel := context.WithTimeout(context.Background(), composeQueryTimeout)
		out, err := d.dockerCmd(stopCtx, "compose", "-p", strings.ToLower(entry.Name()), "stop").CombinedOutput()
		cancel()
		if err != nil {
			d.Log.Warn("sandbox: expiry stop failed", "deployment_id", entry.Name(), "err", err, "out", strings.TrimSpace(string(out)))
			continue
		}
		// Budget spent: drop the marker so the stop is one-shot.
		clearSandboxDeadline(appDir)
	}
}
