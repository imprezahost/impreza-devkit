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

func TestDockerReleaseUninstall(t *testing.T) {
	if os.Getenv("IMPREZA_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires disposable Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := fmt.Sprintf("release_cleanup_%d", time.Now().UnixNano())
	dir := d.appDir(id)
	var tags []string
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		d.forceRemoveProjectContainers(c, id)
		for _, tag := range tags {
			_, _ = d.dockerCmd(c, "image", "rm", tag).CombinedOutput()
		}
	})
	deploy := func(command string) sdkclient.DeployResult {
		compose := fmt.Sprintf("name: %s\nservices:\n  web:\n    image: busybox:1.37.0\n    command: [sh, -c, '%s']\n    restart: always\n", id, command)
		payload, _ := json.Marshal(map[string]any{"deployment_id": id, "manifest": map[string]any{"runtime": map[string]any{"type": "docker-compose", "compose_yaml": compose}}})
		return d.deploy(ctx, &sdkclient.PollCommand{ID: "test_cleanup", Kind: sdkclient.CommandDeploy, Payload: payload})
	}
	for i := 0; i < 2; i++ {
		if r := deploy("sleep 3600"); r.Status != "success" || r.Release == nil {
			t.Fatalf("setup: %+v", r)
		}
	}
	marker := filepath.Join(dir, "data", "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "releases", "*.json"))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var release runtimeRelease
		if err := json.Unmarshal(raw, &release); err != nil {
			t.Fatal(err)
		}
		tags = append(tags, release.Tags...)
	}
	if len(tags) == 0 {
		t.Fatal("no retained tags to test")
	}
	payload, _ := json.Marshal(map[string]any{"deployment_id": id, "purge_data": false})
	if r := d.uninstall(ctx, &sdkclient.PollCommand{ID: "uninstall_test", Kind: sdkclient.CommandUninstall, Payload: payload}); r.Status != "success" {
		t.Fatalf("uninstall: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "releases")); !os.IsNotExist(err) {
		t.Fatal("release secrets retained after uninstall")
	}
	for _, tag := range tags {
		if _, err := d.dockerCmd(ctx, "image", "inspect", tag).Output(); err == nil {
			t.Fatalf("retained image tag leaked: %s", tag)
		}
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("purge_data=false lost data")
	}
	if r := deploy("exit 42"); r.Status != "failed" || r.Rollback != nil {
		t.Fatalf("first install falsely recovered: %+v", r)
	}
	if states, err := d.inspectProjectContainers(ctx, id); err != nil || len(states) != 0 {
		t.Fatalf("failed first install left containers: %v %v", states, err)
	}
}

func TestDockerAutomaticRollback(t *testing.T) {
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
		t.Fatalf("initial release: %+v", first)
	}
	originalImage := imageID()
	if err := os.WriteFile(filepath.Join(dir, "data", "keep"), []byte("persistent"), 0600); err != nil {
		t.Fatal(err)
	}
	writeBuild("new") // The same mutable tag now points at a different image.
	cases := []struct{ name, yaml, hook string }{
		{"crash_loop", strings.Replace(compose, `command: ["sh", "-c", "mkdir -p /www; cp /version /www/index.html; httpd -f -p 8080 -h /www"]`, `command: ["sh", "-c", "exit 42"]`, 1), ""},
		{"unhealthy", strings.Replace(compose, `test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080"]`, `test: ["CMD", "false"]`, 1), ""},
		{"install_hook", compose, "exit 42"},
		{"up_failure", strings.Replace(compose, "restart: always", "restart: always\n    networks: [missing]", 1) + "networks:\n  missing:\n    external: true\n    name: impreza-intentionally-missing-network\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := deploy(tc.yaml, tc.hook)
			if result.Status != "failed" || result.Rollback == nil || result.Rollback.Status != "restored" || result.Rollback.RuntimeState != "healthy" || result.Release == nil {
				t.Fatalf("recovery result: %+v", result)
			}
			if got := httpVersion(); got != "old" {
				t.Fatalf("old version not restored: %s", got)
			}
			if got := imageID(); got != originalImage {
				t.Fatalf("mutable tag used: %s instead of %s", got, originalImage)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "data", "keep")); err != nil || string(got) != "persistent" {
				t.Fatal("lost customer data")
			}
			out, err := d.compose(ctx, dir, "exec", "-T", "web", "printenv", "LITERAL")
			if err != nil || strings.TrimSpace(string(out)) != "value$NOT_AN_ENV" {
				t.Fatalf("literal environment changed: %s %v", out, err)
			}
		})
	}
	previous, err := d.captureRelease(ctx, dir, id)
	if err != nil || previous == nil {
		t.Fatalf("snapshot: %v", err)
	}
	previous.Metadata.ImageIDs["web"] = "sha256:" + strings.Repeat("f", 64)
	failed := d.recoverStartup(ctx, dir, id, previous, true, "test missing recovery image")
	if failed.Rollback == nil || failed.Rollback.Status != "failed" || failed.Rollback.RuntimeState != "unknown" {
		t.Fatalf("missing image falsely recovered: %+v", failed)
	}
	if got := httpVersion(); got != "old" {
		t.Fatal("missing image recovery touched running containers")
	}
	if result := deploy(compose, ""); result.Status != "success" || result.Release == nil {
		t.Fatalf("retry: %+v", result)
	}
	if got := httpVersion(); got != "new" {
		t.Fatalf("retry did not publish new version: %s", got)
	}
	files, err := filepath.Glob(filepath.Join(dir, "releases", "*.json"))
	if err != nil || len(files) > retainedReleases {
		t.Fatalf("unbounded history: %d %v", len(files), err)
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("insecure snapshot permissions: %s", path)
		}
	}
}
