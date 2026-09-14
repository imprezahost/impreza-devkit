package executor

import (
	"context"
	"encoding/json"
	"fmt"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStartupPolicyPersistence(t *testing.T) {
	for _, exists := range []bool{false, true} {
		dir := t.TempDir()
		original := &sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: 120}
		if exists {
			if err := writeStartupPolicy(dir, original); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err := captureDeployConfig(dir, true)
		if err != nil {
			t.Fatal(err)
		}
		if err = writeStartupPolicy(dir, &sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: 30}); err != nil {
			t.Fatal(err)
		}
		if err = snapshot.restore(dir); err != nil {
			t.Fatal(err)
		}
		got, err := readStartupPolicy(dir)
		if err != nil {
			t.Fatal(err)
		}
		if exists && (got == nil || got.TimeoutSeconds != 120) || !exists && got != nil {
			t.Fatalf("policy not restored: %+v", got)
		}
	}
}

func TestDockerRequiredStartup(t *testing.T) {
	if os.Getenv("IMPREZA_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires isolated disposable Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := fmt.Sprintf("required_start_%d", time.Now().UnixNano())
	dir := d.appDir(id)
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		d.forceRemoveProjectContainers(c, id)
		d.pruneReleases(c, filepath.Join(dir, "releases"), 0)
		_, _ = d.dockerCmd(c, "volume", "rm", id+"_kept").CombinedOutput()
	})
	compose := func(delay int, probe string) string {
		s := fmt.Sprintf("name: %s\nservices:\n  web:\n    image: busybox:1.37.0\n    command: [sh, -c, 'echo preserve > /data/marker; sleep %d; touch /ready; sleep 3600']\n    restart: always\n    volumes: [kept:/data]\n", id, delay)
		if probe != "" {
			s += "    healthcheck:\n      test: [CMD, sh, -c, '" + probe + "']\n      interval: 1s\n      timeout: 1s\n      retries: 1\n"
		}
		return s + "volumes:\n  kept: {}\n"
	}
	deploy := func(yaml string, required bool, seconds int) sdkclient.DeployResult {
		rt := map[string]any{"type": "docker-compose", "compose_yaml": yaml}
		if required {
			rt["startup"] = &sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: seconds}
		}
		payload, _ := json.Marshal(map[string]any{"deployment_id": id, "manifest": map[string]any{"runtime": rt}})
		return d.deploy(ctx, &sdkclient.PollCommand{ID: "cmd_required", Kind: sdkclient.CommandDeploy, Payload: payload})
	}
	for _, probe := range []string{"false", ""} {
		r := deploy(compose(0, probe), true, 30)
		if r.Status != "failed" || r.StartupCheck != nil || r.Rollback != nil {
			t.Fatalf("first unhealthy succeeded: %+v", r)
		}
		states, err := d.inspectProjectContainers(ctx, id)
		if err != nil || len(states) != 0 {
			t.Fatalf("first failure left containers: %v %v", states, err)
		}
		out, err := d.dockerCmd(ctx, "run", "--rm", "-v", id+"_kept:/data", "busybox:1.37.0", "cat", "/data/marker").Output()
		if err != nil || strings.TrimSpace(string(out)) != "preserve" {
			t.Fatalf("lost named volume: %s %v", out, err)
		}
	}
	t.Log("first failures remove own containers, preserve named volumes and emit no health receipt")
	legacy := deploy(compose(0, "false"), false, 0)
	if legacy.Status != "success" || legacy.StartupCheck != nil {
		t.Fatalf("legacy advisory changed: %+v", legacy)
	}
	d.forceRemoveProjectContainers(ctx, id)
	started := time.Now()
	first := deploy(compose(65, "test -f /ready"), true, 90)
	if first.Status != "success" || first.StartupCheck == nil || first.StartupCheck.TimeoutSeconds != 90 || first.Release == nil || time.Since(started) < 65*time.Second {
		t.Fatalf("slow first start: %+v", first)
	}
	archive, err := loadRelease(dir, first.Release.ID)
	if err != nil || archive.Startup == nil || archive.Startup.TimeoutSeconds != 90 {
		t.Fatal("release lost policy", err)
	}
	t.Log("slow first application exceeds legacy 60s and succeeds within its 90s policy")
	before, _ := os.ReadFile(filepath.Join(dir, "startup.json"))
	badBuild := strings.Replace(compose(0, "test -f /ready"), "image: busybox:1.37.0", "build: /intentionally-absent-required-start-context", 1)
	if r := deploy(badBuild, true, 30); r.Status != "failed" {
		t.Fatal("bad preparation accepted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "startup.json"))
	if string(before) != string(after) {
		t.Fatal("failed preparation overwrote policy")
	}
	failed := deploy(compose(0, "false"), true, 30)
	if failed.Status != "failed" || failed.Rollback == nil || failed.Rollback.Status != "restored" || failed.Rollback.RuntimeState != "healthy" {
		t.Fatalf("slow recovery failed: %+v", failed)
	}
	saved, err := readStartupPolicy(dir)
	if err != nil || saved == nil || saved.TimeoutSeconds != 90 {
		t.Fatal("automatic recovery lost original policy")
	}
	t.Log("failed replacement recovers original release using its longer startup budget")
	second := deploy(compose(0, "test -f /ready"), true, 30)
	if second.Status != "success" {
		t.Fatalf("fast replacement failed: %+v", second)
	}
	payload, _ := json.Marshal(sdkclient.RollbackPayload{DeploymentID: id, TargetVersion: first.Release.ID})
	r := d.Execute(ctx, &sdkclient.PollCommand{ID: "manual_required", Kind: sdkclient.CommandRollback, Payload: payload})
	if r.Status != "success" || r.Rollback == nil || r.Rollback.RuntimeState != "healthy" {
		t.Fatalf("manual slow rollback: %+v", r)
	}
	saved, err = readStartupPolicy(dir)
	if err != nil || saved == nil || saved.TimeoutSeconds != 90 {
		t.Fatal("manual rollback lost policy")
	}
	t.Log("manual rollback also preserves the selected release startup policy")
}
