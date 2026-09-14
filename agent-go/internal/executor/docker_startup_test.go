package executor

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestRequiredStartupPolicy(t *testing.T) {
	for _, p := range []*sdkclient.ManifestStartup{nil, {}} {
		got, err := resolveStartupPolicy(p)
		if err != nil || got.RequireHealthy || got.Timeout != 60*time.Second || got.receipt(settleHealthy) != nil {
			t.Fatalf("legacy policy changed: %+v %v", got, err)
		}
	}
	for _, seconds := range []int{0, 30, 60, 600} {
		p, err := resolveStartupPolicy(&sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: seconds})
		if err != nil {
			t.Fatal(err)
		}
		r := p.receipt(settleHealthy)
		if r == nil || r.Status != "healthy" || r.Protocol != "startup-health-v1" || p.receipt(settleUnsettled) != nil || p.receipt(settleCrashLooping) != nil {
			t.Fatal("invalid readiness receipt")
		}
	}
	for _, seconds := range []int{-1, 1, 29, 601} {
		if _, err := resolveStartupPolicy(&sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: seconds}); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
	if _, err := resolveStartupPolicy(&sdkclient.ManifestStartup{TimeoutSeconds: 60}); err == nil {
		t.Fatal("timeout accepted without required health")
	}
}

func TestRequiredStartupStates(t *testing.T) {
	cases := []struct {
		name           string
		states         []containerState
		strict, legacy bool
	}{
		{"empty", nil, false, false},
		{"no declared health", []containerState{{Status: "running"}}, false, true},
		{"healthy", []containerState{{Status: "running", Health: "healthy"}}, true, true},
		{"starting", []containerState{{Status: "running", Health: "starting"}}, false, false},
		{"unhealthy", []containerState{{Status: "running", Health: "unhealthy"}}, false, false},
		{"only init", []containerState{{Status: "exited", ExitCode: 0}}, false, true},
		{"healthy and init", []containerState{{Status: "running", Health: "healthy"}, {Status: "exited", ExitCode: 0}}, true, true},
		{"undeclared sidecar", []containerState{{Status: "running", Health: "healthy"}, {Status: "running"}}, false, true},
		{"failed init", []containerState{{Status: "running", Health: "healthy"}, {Status: "exited", ExitCode: 1}}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if startupStatesOK(c.states, true) != c.strict || startupStatesOK(c.states, false) != c.legacy {
				t.Fatal("wrong startup classification")
			}
		})
	}
}

func TestInvalidStartupFailsBeforeStateMutation(t *testing.T) {
	dir := t.TempDir()
	d := &Docker{StateDir: dir}
	payload, _ := json.Marshal(map[string]any{"deployment_id": "invalid_startup", "manifest": map[string]any{"runtime": map[string]any{"type": "docker-compose", "compose_yaml": "services: {}", "startup": map[string]any{"require_healthy": true, "timeout_seconds": 1}}}})
	result := d.deploy(context.Background(), &sdkclient.PollCommand{ID: "cmd_startup", Kind: sdkclient.CommandDeploy, Payload: payload})
	if result.Status != "failed" || result.CommandID != "cmd_startup" || result.StartupCheck != nil {
		t.Fatalf("invalid policy did not fail: %+v", result)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("invalid startup mutated state")
	}
}
