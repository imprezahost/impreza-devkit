package executor

// Startup protocol v2 behavior: the release capture judges
// the running stack with the startup protocol that release was deployed
// with — a v2 application without a healthcheck is a valid recovery
// target — and the v2 stability window is behavior, not just a predicate:
// a restart inside the window resets it, so a flapping service never
// becomes ready.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// TestCaptureReleaseUsesTheDeployedStartupProtocol: an application deployed
// under startup-health-v2 without a healthcheck, running and serving, is an
// eligible recovery target. Judging it with the v1 predicate returns no
// release — its second failing deploy would have nothing to roll back to.
func TestCaptureReleaseUsesTheDeployedStartupProtocol(t *testing.T) {
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := "dpl_v2rel000000000a1"
	dir := d.appDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: busybox:1.37.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "startup.json"), []byte(`{"require_healthy":true,"timeout_seconds":60,"protocol":"startup-health-v2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	web := dockertest.ComposeContainer(id, "web", "running", 0)
	state := dockertest.State{Containers: []dockertest.Container{web}, ComposeConfig: map[string]string{}}
	state.ComposeConfig[dir] = `{"name":"` + id + `","services":{"web":{"image":"busybox:1.37.0"}}}`
	dockertest.Install(t, state)

	release, err := d.captureRelease(context.Background(), dir, id)
	if err != nil {
		t.Fatalf("capture failed: %v", err)
	}
	if release == nil {
		t.Fatal("a running v2 stack without a healthcheck was not captured as a recovery target (its next failing deploy would have no rollback)")
	}
	if release.Startup == nil || release.Startup.Protocol != StartupProtocolV2 {
		t.Fatalf("the captured release must carry its own v2 startup policy: %+v", release.Startup)
	}
}

// bumpRestarts rewrites the double's state adding one restart to the
// deployment's containers, the way a flapping service looks to Docker.
func bumpRestarts(t *testing.T, statePath, project string) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state dockertest.State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	for i := range state.Containers {
		if state.Containers[i].Config.Labels["com.docker.compose.project"] == project {
			state.Containers[i].RestartCount++
		}
	}
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	tmp := statePath + ".flap"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, statePath); err != nil {
		t.Fatal(err)
	}
}

// TestStartupV2WindowResetsOnRestart: a healthcheck-less v2 service that
// keeps restarting never accumulates the stability window, and the same
// stack with no restarts does. Removing the window reset makes the
// flapping phase report healthy.
func TestStartupV2WindowResetsOnRestart(t *testing.T) {
	original := v2StableWindow
	v2StableWindow = 9 * time.Second // three samples of the fixed interval
	t.Cleanup(func() { v2StableWindow = original })
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := "dpl_v2win000000000a1"
	web := dockertest.ComposeContainer(id, "web", "running", 0)
	double := dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{web}})
	policy := startupPolicy{RequireHealthy: true, V2: true, Timeout: 21 * time.Second}

	stopFlap := make(chan struct{})
	flapDone := make(chan struct{})
	go func() {
		defer close(flapDone)
		// Faster than the settle interval: every sample must see a restart
		// that happened after its baseline.
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopFlap:
				return
			case <-ticker.C:
				bumpRestarts(t, double.StatePath(), id)
			}
		}
	}()
	verdict, why := d.awaitStackSettledPolicy(context.Background(), id, policy)
	close(stopFlap)
	<-flapDone
	if verdict == settleHealthy {
		t.Fatalf("a service that restarts inside every stability window was called healthy: %s", why)
	}

	// The same stack, left alone, settles through the window.
	verdict, why = d.awaitStackSettledPolicy(context.Background(), id, policy)
	if verdict != settleHealthy {
		t.Fatalf("a stable healthcheck-less v2 stack did not become ready: %s", why)
	}
}

// TestDockerAutomaticRollbackV2WithoutHealthcheck: the full path — a v2
// application without a healthcheck, a second deploy that fails, and the
// rollback to the first release. When the capture drops the v2 stack, the
// failed deploy reports no rollback and the application stays down.
func TestDockerAutomaticRollbackV2WithoutHealthcheck(t *testing.T) {
	if os.Getenv("IMPREZA_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires disposable Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := fmt.Sprintf("v2rollback%d", time.Now().UnixNano())
	dir := d.appDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		d.compose(c, dir, "down", "--volumes", "--remove-orphans")
		files, _ := filepath.Glob(filepath.Join(dir, "releases", "*.json"))
		for _, path := range files {
			raw, _ := os.ReadFile(path)
			var r runtimeRelease
			if json.Unmarshal(raw, &r) == nil {
				for _, tag := range r.Tags {
					_, _ = d.dockerCmd(c, "image", "rm", tag).CombinedOutput()
				}
			}
		}
		_, _ = d.dockerCmd(c, "image", "rm", id+":app").CombinedOutput()
	})
	writeBuild := func(version string) {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:1.37.0\nRUN echo "+version+" > /version\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	compose := fmt.Sprintf(`name: %s
services:
  web:
    image: %s:app
    build: .
    command: ["sh", "-c", "mkdir -p /www; cp /version /www/index.html; httpd -f -p 8080 -h /www"]
    restart: always
    volumes: ["./data:/data"]
`, id, id)
	deploy := func(hook string) sdkclient.DeployResult {
		body, err := json.Marshal(map[string]any{"deployment_id": id,
			"manifest": map[string]any{"runtime": map[string]any{"type": "docker-compose", "compose_yaml": compose,
				"startup": map[string]any{"require_healthy": true, "timeout_seconds": 60, "protocol": "startup-health-v2"}},
				"lifecycle": map[string]any{"install": hook}}})
		if err != nil {
			t.Fatal(err)
		}
		return d.deploy(ctx, &sdkclient.PollCommand{ID: "cmd_v2rb", Kind: sdkclient.CommandDeploy, Payload: body})
	}
	serving := func() string {
		out, err := d.compose(ctx, dir, "exec", "-T", "web", "cat", "/version")
		if err != nil {
			return "down: " + err.Error()
		}
		return strings.TrimSpace(string(out))
	}
	writeBuild("old")
	first := deploy("")
	if first.Status != "success" || first.Release == nil {
		t.Fatalf("initial v2 release: %+v", first)
	}
	if got := serving(); got != "old" {
		t.Fatalf("the first release is not serving: %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "keep"), []byte("persistent"), 0600); err != nil {
		t.Fatal(err)
	}
	writeBuild("new")
	failed := deploy("exit 42")
	if failed.Status != "failed" || failed.Rollback == nil || failed.Rollback.Status != "restored" || failed.Release == nil {
		t.Fatalf("the failing v2 deploy did not roll back: %+v", failed)
	}
	if got := serving(); got != "old" {
		t.Fatalf("the first version was not restored: %s", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "data", "keep")); err != nil || string(got) != "persistent" {
		t.Fatal("lost customer data")
	}
}
