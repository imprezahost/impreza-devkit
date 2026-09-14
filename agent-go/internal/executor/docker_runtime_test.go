package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeVerdict(t *testing.T) {
	tests := []struct {
		name       string
		services   []string
		containers []runtimeContainer
		want       string
	}{
		{"empty", []string{"web"}, nil, "stopped"},
		{"healthy", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running", Health: "healthy"}}, "healthy"},
		{"no check", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running"}}, "running"},
		{"unhealthy", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running", Health: "unhealthy"}}, "degraded"},
		{"starting", []string{"web"}, []runtimeContainer{{Service: "web", Status: "running", Health: "starting"}}, "starting"},
		{"missing service", []string{"web", "db"}, []runtimeContainer{{Service: "db", Status: "running", Health: "healthy"}}, "degraded"},
		{"stopped web", []string{"web", "db"}, []runtimeContainer{{Service: "db", Status: "running", Health: "healthy"}, {Service: "web", Status: "exited"}}, "degraded"},
		{"stopped", []string{"web"}, []runtimeContainer{{Service: "web", Status: "exited"}}, "stopped"},
		{"crash", []string{"web"}, []runtimeContainer{{Service: "web", Status: "exited", ExitCode: 1}}, "degraded"},
		{"paused", []string{"web"}, []runtimeContainer{{Service: "web", Status: "paused"}}, "degraded"},
		{"restart loop", []string{"web"}, []runtimeContainer{{Service: "web", Status: "restarting"}}, "degraded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, state := runtimeVerdict(tt.services, tt.containers)
			if state != tt.want {
				t.Fatalf("got %s (%+v), want %s", state, c, tt.want)
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
