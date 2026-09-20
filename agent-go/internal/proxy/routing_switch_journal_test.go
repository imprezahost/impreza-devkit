package proxy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSwitchProcessInterruption(t *testing.T) {
	const childEnv = "IMPREZA_ROUTING_CRASH_TEST_STATE"
	source, target := "dpl_"+strings.Repeat("a", 16), "dpl_"+strings.Repeat("b", 16)
	if dir := os.Getenv(childEnv); dir != "" {
		c := &Caddy{StateDir: dir, switchReload: func(context.Context) error { os.Exit(77); return nil }}
		_, _ = c.SwitchHostname(context.Background(), "app.example.test", source, target, target+"-app:8080")
		os.Exit(78)
	}
	c := &Caddy{StateDir: t.TempDir()}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	original := []byte("app.example.test {\n  reverse_proxy " + source + "-app:8080\n}\n")
	sourcePath := filepath.Join(c.StateDir, "deployments", source+".caddy")
	if err := os.WriteFile(sourcePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSwitchProcessInterruption$")
	cmd.Env = append(os.Environ(), childEnv+"="+c.StateDir)
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 77 {
		t.Fatalf("crash injection failed: %v", err)
	}
	raw, err := os.ReadFile(c.switchRecordPath())
	if err != nil {
		t.Fatal(err)
	}
	var saved routingSwitchRecord
	if json.Unmarshal(raw, &saved) != nil || string(saved.SourceBefore) != string(original) || saved.TargetExisted {
		t.Fatal("recovery evidence missing")
	}
	// Re-open as a fresh agent process would. No unrelated operation can reload
	// partial routing or erase its recovery evidence.
	fresh := &Caddy{StateDir: c.StateDir}
	for name, call := range map[string]func() error{
		"ensure":           func() error { return fresh.EnsureRunning(t.Context()) },
		"unrelated remove": func() error { return fresh.RemoveDeploymentRoutes(t.Context(), "dpl_"+strings.Repeat("c", 16)) },
		"source update":    func() error { return fresh.ApplyDeploymentRoutes(t.Context(), source, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("interrupted routing accepted a mutation")
			}
		})
	}
	if err := fresh.CompleteHostnameSwitch("other.example.test", source, target); err == nil {
		t.Fatal("another identity cleared recovery")
	}
	if _, err := os.Stat(c.switchRecordPath()); err != nil {
		t.Fatal("recovery evidence erased")
	}
}
