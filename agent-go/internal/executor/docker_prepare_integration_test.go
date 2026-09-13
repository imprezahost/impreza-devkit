package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Run only on an isolated disposable Docker host, never a customer server.
func TestDockerPreparationPreservesRunningDeployment(t *testing.T) {
	if os.Getenv("IMPREZA_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires disposable Linux Docker host and IMPREZA_DOCKER_TEST=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := fmt.Sprintf("prep_test_%d", time.Now().UnixNano())
	dir := d.appDir(id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	compose := fmt.Sprintf(`name: %s
services:
  web:
    image: busybox:1.37.0
    command: ["sh", "-c", "mkdir -p /www; echo $$VERSION > /www/index.html; httpd -f -p 8080 -h /www"]
    environment:
      VERSION: ${VERSION}
    ports: ["127.0.0.1::8080"]
    volumes: ["./data:/data"]
`, id)
	deploy := func(yaml, version string) sdkclient.DeployResult {
		payload, err := json.Marshal(map[string]any{"deployment_id": id, "manifest": map[string]any{"runtime": map[string]any{"type": "docker-compose", "compose_yaml": yaml}}, "vars": map[string]any{"VERSION": version}})
		if err != nil {
			t.Fatal(err)
		}
		return d.deploy(ctx, &sdkclient.PollCommand{ID: "test_" + version, Kind: sdkclient.CommandDeploy, Payload: payload})
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		out, err := d.compose(c, dir, "down", "--volumes", "--remove-orphans", "--rmi", "local")
		if err != nil {
			t.Errorf("cleanup: %v %s", err, out)
		}
	})
	if r := deploy(compose, "old"); r.Status != "success" {
		t.Fatalf("initial deployment: %+v", r)
	}
	containerID := func() string {
		out, err := d.compose(ctx, dir, "ps", "-q", "web")
		if err != nil {
			t.Fatalf("container lookup: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	initialID := containerID()
	if initialID == "" {
		t.Fatal("no initial container")
	}
	out, err := d.compose(ctx, dir, "port", "web", "8080")
	if err != nil {
		t.Fatalf("port: %v %s", err, out)
	}
	endpoint := "http://" + strings.TrimSpace(string(out))
	readHTTP := func() string {
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(body))
	}
	if got := readHTTP(); got != "old" {
		t.Fatalf("initial HTTP: %s", got)
	}
	marker := filepath.Join(dir, "data", "keep.txt")
	if err := os.WriteFile(marker, []byte("customer data"), 0600); err != nil {
		t.Fatal(err)
	}
	previousCompose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	previousEnv, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:1.37.0\nRUN exit 42\n"), 0600); err != nil {
		t.Fatal(err)
	}
	buildCompose := strings.Replace(compose, "image: busybox:1.37.0", "build: .", 1)
	for _, tc := range []struct{ name, yaml string }{
		{"invalid_compose", "services: [invalid"},
		{"failed_pull", strings.Replace(compose, "busybox:1.37.0", "busybox:impreza-intentionally-missing-test-tag", 1)},
		{"failed_build", buildCompose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Observe availability while preparation is actually running.
			done := make(chan sdkclient.DeployResult, 1)
			go func() { done <- deploy(tc.yaml, "new") }()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			var result sdkclient.DeployResult
			waiting := true
			for waiting {
				select {
				case result = <-done:
					waiting = false
				case <-ticker.C:
					if got := readHTTP(); got != "old" {
						t.Fatalf("HTTP changed during preparation: %s", got)
					}
				}
			}
			if result.Status == "success" || !strings.Contains(result.Error, "previous containers and configuration were preserved") {
				t.Fatalf("expected preserved failed attempt: %+v", result)
			}
			if got := containerID(); got != initialID {
				t.Fatalf("container replaced: %s -> %s", initialID, got)
			}
			if got := readHTTP(); got != "old" {
				t.Fatalf("old application unavailable: %s", got)
			}
			for name, want := range map[string]string{"compose.yaml": string(previousCompose), ".env": string(previousEnv), "data/keep.txt": "customer data"} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(got) != want {
					t.Fatalf("%s changed: %q %v", name, got, err)
				}
			}
		})
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:1.37.0\nRUN echo build-success > /build-proof\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := deploy(buildCompose, "new"); r.Status != "success" {
		t.Fatalf("successful retry: %+v", r)
	}
	if containerID() == initialID {
		t.Fatal("successful retry did not replace container")
	}
	out, err = d.compose(ctx, dir, "port", "web", "8080")
	if err != nil {
		t.Fatal(err)
	}
	endpoint = "http://" + strings.TrimSpace(string(out))
	if got := readHTTP(); got != "new" {
		t.Fatalf("retry did not serve new version: %s", got)
	}
}
