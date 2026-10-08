package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const (
	// StartupProtocolV1 keeps the v1 gate: a service without a healthcheck
	// never passes the required startup check.
	StartupProtocolV1 = "startup-health-v1"
	// StartupProtocolV2 treats a healthcheck-less service as ready once
	// it is running and the settle gate has seen it stable — the same
	// semantics as `docker compose up --wait`. A service with a
	// healthcheck still has to reach `healthy` under either protocol.
	StartupProtocolV2 = "startup-health-v2"
)

// v2StableWindow is the observation period a healthcheck-less service
// must survive without a restart before the v2 gate calls it ready.
// A service that crash-loops every few seconds stays inside the
// window and never becomes ready; the compose `--wait` default of
// 60s is too slow for a deploy, and 3 samples (9s) misses a loop
// that fires every 12s (measured on a test host). 15s catches both
// while keeping the gate responsive. Var so tests shrink it.
var v2StableWindow = 15 * time.Second

type startupPolicy struct {
	RequireHealthy bool
	Timeout        time.Duration
	// V2 is true when the manifest explicitly asked for the v2 protocol.
	// It changes two things: a service without a healthcheck becomes
	// ready when stable, and the stability bar is the v2StableWindow
	// with restart-count tracking, not just the settle samples.
	V2       bool
	Protocol string
}

func resolveStartupPolicy(p *sdkclient.ManifestStartup) (startupPolicy, error) {
	result := startupPolicy{Timeout: settleBudget, Protocol: StartupProtocolV1}
	if p == nil {
		return result, nil
	}
	if !p.RequireHealthy {
		if p.TimeoutSeconds != 0 {
			return result, fmt.Errorf("startup timeout requires require_healthy=true")
		}
		if p.Protocol != "" && p.Protocol != StartupProtocolV1 {
			return result, fmt.Errorf("startup protocol requires require_healthy=true")
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
	policy := startupPolicy{RequireHealthy: true, Timeout: time.Duration(seconds) * time.Second, Protocol: StartupProtocolV1}
	switch p.Protocol {
	case "", StartupProtocolV1:
	case StartupProtocolV2:
		policy.V2 = true
		policy.Protocol = StartupProtocolV2
	default:
		return result, fmt.Errorf("startup protocol %q is not supported", p.Protocol)
	}
	return policy, nil
}

// startupStatesOK is the shared "is this stack serving" predicate.
func startupStatesOK(states []containerState, required bool) bool {
	return startupStatesOKProtocol(states, required, false)
}

// hasHealthcheck reports whether any running service in the set declares
// a Docker healthcheck. The v2 stability window only applies to stacks
// without one: a stack that declares a check still waits for `healthy`.
func hasHealthcheck(states []containerState) bool {
	for _, s := range states {
		if s.Health != "" {
			return true
		}
	}
	return false
}

// everyServiceHasHealthcheck reports whether each service in the set
// declares one. The v2 stability window applies to any stack where some
// service does not: its readiness cannot be confirmed by a health probe,
// only by surviving the window without a restart.
func everyServiceHasHealthcheck(states []containerState) bool {
	if len(states) == 0 {
		return false
	}
	for _, s := range states {
		if s.Health == "" {
			return false
		}
	}
	return true
}

func startupStatesOKProtocol(states []containerState, required, v2 bool) bool {
	if len(states) == 0 {
		return false
	}
	servingService := false
	for _, s := range states {
		if !s.ok() {
			return false
		}
		if s.Status == "running" {
			if required && !v2 && s.Health == "" {
				// v1: no healthcheck means the gate cannot confirm the
				// service is ready.
				return false
			}
			servingService = true
		}
	}
	// Successful init jobs alone are not a serving application.
	return !required || servingService
}

func (p startupPolicy) receipt(verdict settleVerdict) *sdkclient.DeploymentStartupCheck {
	if !p.RequireHealthy || verdict != settleHealthy {
		return nil
	}
	return &sdkclient.DeploymentStartupCheck{Protocol: p.Protocol, Status: "healthy", TimeoutSeconds: int(p.Timeout / time.Second)}
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
