package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestDockerManualRollback(t *testing.T) {
	if os.Getenv("IMPREZA_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires isolated disposable Linux Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := fmt.Sprintf("rollback_test_%d", time.Now().UnixNano())
	dir := d.appDir(id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	compose := fmt.Sprintf(`name: %s
services:
  web:
    image: %s:app
    build: .
    command: ["sh", "-c", "mkdir -p /www; cp /version /www/index.html; httpd -f -p 8080 -h /www"]
    restart: always
    environment:
      LITERAL: 'value$$NOT_AN_ENV'
    ports: ["127.0.0.1:%d:8080"]
    volumes: ["./data:/data"]
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080"]
      interval: 1s
      timeout: 1s
      retries: 1
`, id, id, port)
	writeBuild := func(version string) {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:1.37.0\nRUN echo "+version+" > /version\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deploy := func(yaml, hook string) sdkclient.DeployResult {
		body, err := json.Marshal(map[string]any{"deployment_id": id, "manifest": map[string]any{"runtime": map[string]any{"type": "docker-compose", "compose_yaml": yaml}, "lifecycle": map[string]any{"install": hook}}, "vars": map[string]any{"VERSION": "test"}})
		if err != nil {
			t.Fatal(err)
		}
		return d.deploy(ctx, &sdkclient.PollCommand{ID: "cmd_test", Kind: sdkclient.CommandDeploy, Payload: body})
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		out, err := d.compose(c, dir, "down", "--volumes", "--remove-orphans")
		if err != nil {
			t.Errorf("cleanup: %v %s", err, out)
		}
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
	httpVersion := func() string {
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(raw))
	}
	imageID := func() string {
		ids, err := d.compose(ctx, dir, "ps", "-q", "web")
		if err != nil {
			t.Fatal(err)
		}
		out, err := d.dockerCmd(ctx, "inspect", "--format", "{{.Image}}", strings.TrimSpace(string(ids))).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	writeBuild("old")
	first := deploy(compose, "")
	if first.Status != "success" || first.Release == nil {
		t.Fatalf("first: %+v", first)
	}
	originalImage := imageID()
	if first.Release.RollbackProtocol != "release-v1" {
		t.Fatal("missing capability")
	}
	writeBuild("new")
	second := deploy(compose, "")
	if second.Status != "success" {
		t.Fatalf("second: %+v", second)
	}
	if httpVersion() != "new" {
		t.Fatal("new not running")
	}
	for i := 0; i < 2; i++ {
		if _, err := d.captureRelease(ctx, dir, id); err != nil {
			t.Fatal(err)
		}
	}
	rollback := func(target string) sdkclient.DeployResult {
		payload, _ := json.Marshal(sdkclient.RollbackPayload{DeploymentID: id, TargetVersion: target})
		return d.Execute(ctx, &sdkclient.PollCommand{ID: "manual", Kind: sdkclient.CommandRollback, Payload: payload})
	}
	result := rollback(first.Release.ID)
	if result.Status != "success" || result.Rollback == nil || result.Rollback.ReleaseID != first.Release.ID {
		t.Fatalf("manual: %+v", result)
	}
	if httpVersion() != "old" || imageID() != originalImage {
		t.Fatal("immutable release not restored")
	}
	if _, err := loadRelease(dir, first.Release.ID); err != nil {
		t.Fatal("selected oldest release pruned", err)
	}
	before, _ := d.compose(ctx, dir, "ps", "-q", "web")
	for _, target := range []string{"rel_expired", "../escape"} {
		if r := rollback(target); r.Status != "failed" {
			t.Fatalf("invalid accepted: %+v", r)
		}
	}
	archive := filepath.Join(dir, "releases", first.Release.ID+".json")
	saved, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot runtimeRelease
	if err := json.Unmarshal(saved, &snapshot); err != nil {
		t.Fatal(err)
	}
	var model map[string]any
	json.Unmarshal(snapshot.Compose, &model)
	web := model["services"].(map[string]any)["web"].(map[string]any)
	web["ports"] = []any{}
	snapshot.Compose, _ = json.Marshal(model)
	changed, _ := json.Marshal(snapshot)
	os.WriteFile(archive, changed, 0600)
	if r := rollback(first.Release.ID); r.Status != "failed" || !strings.Contains(r.Error, "ports, storage") {
		t.Fatalf("contract accepted: %+v", r)
	}
	os.WriteFile(archive, []byte("corrupt"), 0600)
	if r := rollback(first.Release.ID); r.Status != "failed" {
		t.Fatal("corrupt accepted")
	}
	os.WriteFile(archive, saved, 0600)
	after, _ := d.compose(ctx, dir, "ps", "-q", "web")
	if string(before) != string(after) {
		t.Fatal("rejection replaced container")
	}
	// A snapshot may become unhealthy after mutable data changes. Recover the current runtime.
	json.Unmarshal(saved, &snapshot)
	json.Unmarshal(snapshot.Compose, &model)
	web = model["services"].(map[string]any)["web"].(map[string]any)
	web["command"] = []string{"sh", "-c", "exit 42"}
	snapshot.Compose, _ = json.Marshal(model)
	changed, _ = json.Marshal(snapshot)
	os.WriteFile(archive, changed, 0600)
	failed := rollback(first.Release.ID)
	if failed.Status != "failed" || failed.Rollback == nil || failed.Rollback.Status != "restored" {
		t.Fatalf("failed rollback recovery: %+v", failed)
	}
	if httpVersion() != "old" {
		t.Fatal("current runtime not recovered")
	}
}
