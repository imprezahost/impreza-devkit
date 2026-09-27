package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestRuntimeVerdict(t *testing.T) {
	tests := []struct {
		name       string
		services   []string
		containers []runtimeContainer
		want       string
		wantCounts sdkclient.RuntimeCounts
	}{
		{"empty", []string{"web"}, nil, "stopped", sdkclient.RuntimeCounts{ExpectedServices: 1, MissingServices: 1}},
		{"healthy", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running", Health: "healthy"}}, "healthy", sdkclient.RuntimeCounts{Total: 1, Running: 1, Healthy: 1, ExpectedServices: 1}},
		{"no check", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running"}}, "running", sdkclient.RuntimeCounts{Total: 1, Running: 1, ExpectedServices: 1}},
		{"unhealthy", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running", Health: "unhealthy"}}, "degraded", sdkclient.RuntimeCounts{Total: 1, Running: 1, Unhealthy: 1, ExpectedServices: 1}},
		{"starting", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running", Health: "starting"}}, "starting", sdkclient.RuntimeCounts{Total: 1, Running: 1, Starting: 1, ExpectedServices: 1}},
		{"missing service", []string{"web", "db"}, []runtimeContainer{{Service: "db", Status: "running", Health: "healthy"}}, "degraded", sdkclient.RuntimeCounts{Total: 1, Running: 1, Healthy: 1, ExpectedServices: 2, MissingServices: 1}},
		// An UNMARKED service that exited 0 — a long-running
		// service parked with `docker stop` exits 0 too — is an unexpected
		// stop, never a completion. The stack must degrade, not heal.
		{"web stopped via docker stop beside healthy db", []string{"web", "db"}, []runtimeContainer{{Service: "db", Status: "running", Health: "healthy"}, {Service: "web", Status: "exited"}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Healthy: 1, Stopped: 1, ExpectedServices: 2}},
		{"web crashed beside healthy db", []string{"web", "db"}, []runtimeContainer{{Service: "db", Status: "running", Health: "healthy"}, {Service: "web", Status: "exited", ExitCode: 1}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Healthy: 1, Stopped: 1, Failed: 1, ExpectedServices: 2}},
		{"stopped", []string{"web"}, []runtimeContainer{{Service: "web", Status: "exited"}}, "stopped", sdkclient.RuntimeCounts{Total: 1, Stopped: 1, ExpectedServices: 1}},
		{"crash", []string{"web"}, []runtimeContainer{{Service: "web", Status: "exited", ExitCode: 1}}, "degraded", sdkclient.RuntimeCounts{Total: 1, Stopped: 1, Failed: 1, ExpectedServices: 1}},
		{"paused", []string{"web"}, []runtimeContainer{{Service: "web", Status: "paused"}}, "degraded", sdkclient.RuntimeCounts{Total: 1, Failed: 1, ExpectedServices: 1}},
		{"restart loop", []string{"web"}, []runtimeContainer{{Service: "web", Status: "restarting"}}, "degraded", sdkclient.RuntimeCounts{Total: 1, Failed: 1, ExpectedServices: 1}},
		// A DECLARED one-shot init job (another service depends on it
		// with condition service_completed_successfully — the tls-init and
		// synapse-init shape) that exited 0 must not degrade a stack whose
		// long-running services are up — the settle gate accepts it, and
		// the runtime collector has to agree.
		// The label is what Compose writes: service:condition:restart
		// entries joined by ',' (measured on compose 2.40.3).
		{"one-shot init healthy", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", Health: "healthy", DependsOn: "init:service_completed_successfully:false"}, {Service: "init", Status: "exited"}}, "healthy", sdkclient.RuntimeCounts{Total: 2, Running: 1, Healthy: 1, Stopped: 1, Completed: 1, ExpectedServices: 2}},
		{"one-shot init running", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", DependsOn: "init:service_completed_successfully:false"}, {Service: "init", Status: "exited"}}, "running", sdkclient.RuntimeCounts{Total: 2, Running: 1, Stopped: 1, Completed: 1, ExpectedServices: 2}},
		{"one-shot among several dependencies", []string{"app", "db", "init"}, []runtimeContainer{{Service: "app", Status: "running", DependsOn: "db:service_healthy:true,init:service_completed_successfully:false"}, {Service: "db", Status: "running", Health: "healthy"}, {Service: "init", Status: "exited"}}, "running", sdkclient.RuntimeCounts{Total: 3, Running: 2, Healthy: 1, Stopped: 1, Completed: 1, ExpectedServices: 3}},
		{"one-shot failed init", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", DependsOn: "init:service_completed_successfully:false"}, {Service: "init", Status: "exited", ExitCode: 1}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Stopped: 1, Failed: 1, ExpectedServices: 2}},
		{"one-shot mark other condition", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", DependsOn: "init:service_started:false"}, {Service: "init", Status: "exited"}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Stopped: 1, ExpectedServices: 2}},
		{"one-shot mark without condition", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", DependsOn: "init"}, {Service: "init", Status: "exited"}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Stopped: 1, ExpectedServices: 2}},
		{"one-shot unparsable mark", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", DependsOn: `{not json`}, {Service: "init", Status: "exited"}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Stopped: 1, ExpectedServices: 2}},
		// A JSON object is accepted defensively.
		{"one-shot json label shape", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", Health: "healthy", DependsOn: `{"init":{"condition":"service_completed_successfully","required":true}}`}, {Service: "init", Status: "exited"}}, "healthy", sdkclient.RuntimeCounts{Total: 2, Running: 1, Healthy: 1, Stopped: 1, Completed: 1, ExpectedServices: 2}},
		{"one-shot json other condition", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running", DependsOn: `{"init":{"condition":"service_started","required":true}}`}, {Service: "init", Status: "exited"}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Stopped: 1, ExpectedServices: 2}},
		{"created not started", []string{"app", "init"}, []runtimeContainer{{Service: "app", Status: "running"}, {Service: "init", Status: "created"}}, "degraded", sdkclient.RuntimeCounts{Total: 2, Running: 1, Stopped: 1, ExpectedServices: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, state := runtimeVerdict(tt.services, tt.containers)
			if state != tt.want {
				t.Fatalf("got %s (%+v), want %s", state, c, tt.want)
			}
			if c != tt.wantCounts {
				t.Fatalf("counts got %+v, want %+v", c, tt.wantCounts)
			}
		})
	}
}
func TestRuntimeCollectorBoundaries(t *testing.T) {
	d := &Docker{StateDir: t.TempDir()}
	if s := d.CollectRuntime(context.Background()); !s.Complete || len(s.Deployments) != 0 {
		t.Fatal(s)
	}
	if err := os.MkdirAll(filepath.Join(d.StateDir, "apps", "not-managed"), 0700); err != nil {
		t.Fatal(err)
	}
	if s := d.CollectRuntime(context.Background()); len(s.Deployments) != 0 {
		t.Fatal("unmanaged folder sampled")
	}
	if err := os.MkdirAll(filepath.Join(d.StateDir, "apps", "dpl_cancelled"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	s := d.CollectRuntime(ctx)
	if s.Complete || len(s.Deployments) != 0 || time.Since(started) > time.Second {
		t.Fatal("cancelled collection must be partial and bounded", s)
	}
}

func TestRuntimeOutputHelper(t *testing.T) {
	switch os.Getenv("IMPREZA_RUNTIME_HELPER") {
	case "large":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 300000))
		os.Exit(0)
	case "slow":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}
func TestRuntimeOutputIsBounded(t *testing.T) {
	for _, mode := range []string{"large", "slow"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeOutputHelper$")
			cmd.Env = append(os.Environ(), "IMPREZA_RUNTIME_HELPER="+mode)
			started := time.Now()
			data, err := limitedRuntimeOutput(cmd, 1024)
			if err == nil || len(data) > 1024 || time.Since(started) > 3*time.Second {
				t.Fatalf("unbounded output: %d, %v", len(data), err)
			}
		})
	}
}
