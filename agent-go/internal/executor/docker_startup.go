package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

type startupPolicy struct {
	RequireHealthy bool
	Timeout        time.Duration
}

func resolveStartupPolicy(p *sdkclient.ManifestStartup) (startupPolicy, error) {
	result := startupPolicy{Timeout: settleBudget}
	if p == nil {
		return result, nil
	}
	if !p.RequireHealthy {
		if p.TimeoutSeconds != 0 {
			return result, fmt.Errorf("startup timeout requires require_healthy=true")
		}
		return result, nil
	}
	seconds := p.TimeoutSeconds
	if seconds == 0 {
		seconds = 60
	}
	if seconds < 30 || seconds > 600 {
		return result, fmt.Errorf("startup timeout_seconds must be between 30 and 600")
	}
	return startupPolicy{RequireHealthy: true, Timeout: time.Duration(seconds) * time.Second}, nil
}

func startupStatesOK(states []containerState, required bool) bool {
	if len(states) == 0 {
		return false
	}
	healthyService := false
	for _, s := range states {
		if !s.ok() {
			return false
		}
		if s.Status == "running" {
			if required && s.Health != "healthy" {
				return false
			}
			healthyService = true
		}
	}
	// Successful init jobs alone are not a serving application.
	return !required || healthyService
}

func (p startupPolicy) receipt(verdict settleVerdict) *sdkclient.DeploymentStartupCheck {
	if !p.RequireHealthy || verdict != settleHealthy {
		return nil
	}
	return &sdkclient.DeploymentStartupCheck{Protocol: "startup-health-v1", Status: "healthy", TimeoutSeconds: int(p.Timeout / time.Second)}
}

// Persist with the deployment configuration so each retained release recovers
// with its own health deadline, including manual rollback and slow starts.
func readStartupPolicy(dir string) (*sdkclient.ManifestStartup, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "startup.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var policy sdkclient.ManifestStartup
	if err = json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	if _, err = resolveStartupPolicy(&policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func writeStartupPolicy(dir string, policy *sdkclient.ManifestStartup) error {
	if _, err := resolveStartupPolicy(policy); err != nil {
		return err
	}
	path := filepath.Join(dir, "startup.json")
	if policy == nil || !policy.RequireHealthy {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw, 0600)
}

func releaseStartupBudget(release *runtimeRelease) time.Duration {
	if release != nil {
		if p, err := resolveStartupPolicy(release.Startup); err == nil {
			return p.Timeout
		}
	}
	return settleBudget
}
