package executor

import (
	"context"
	"encoding/json"
	"fmt"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDockerPinnedGitPreservesRuntime(t *testing.T) {
	if os.Getenv("IMPREZA_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires disposable Linux Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cli, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "fixture", AgentSecret: "fixture", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	d := &Docker{StateDir: t.TempDir(), Client: cli, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := fmt.Sprintf("git_pin_%d", time.Now().UnixNano())
	dir := d.appDir(id)
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		d.forceRemoveProjectContainers(c, id)
	})
	// Serve a file copied from the pinned public Git source into the container.
	// The current public branch contains 0.12.0, while the requested older commit
	// contains 0.11.0. This exercises a real HTTPS clone and exact-object fetch.
	compose := fmt.Sprintf(`name: %s
services:
  web:
    image: busybox:1.37.0
    command: [sh, -c, 'mkdir -p /www; cp /source/package.json /www/index.html; httpd -f -p 8080 -h /www']
    ports: ["127.0.0.1::8080"]
    volumes: ["./build-ctx:/source:ro", "./data:/data"]
`, id)
	deploy := func(sha string) sdkclient.DeployResult {
		payload, _ := json.Marshal(map[string]any{"deployment_id": id, "git_commit_sha": sha, "manifest": map[string]any{"runtime": map[string]any{"type": "docker-compose", "compose_yaml": compose, "build": map[string]any{"git": map[string]any{"url": "https://github.com/imprezahost/impreza-mcp.git", "ref": "main"}}}}})
		return d.deploy(ctx, &sdkclient.PollCommand{ID: "git_fixture", Kind: sdkclient.CommandDeploy, Payload: payload})
	}
	old := "2ebfb22ddc0079bf9e1a67d053c559d6fea5edb4"
	if r := deploy(old); r.Status != "success" {
		t.Fatalf("pinned source: %+v", r)
	}
	out, err := d.compose(ctx, dir, "port", "web", "8080")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + strings.TrimSpace(string(out))
	read := func() {
		t.Helper()
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var p struct{ Version string }
		if err = json.NewDecoder(resp.Body).Decode(&p); err != nil || p.Version != "0.11.0" {
			t.Fatalf("wrong source served: %s %v", p.Version, err)
		}
	}
	read()
	before, _ := d.compose(ctx, dir, "ps", "-q", "web")
	marker := filepath.Join(dir, "data", "keep")
	if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	previous, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	for _, sha := range []string{strings.Repeat("0", 40), "--invalid"} {
		done := make(chan sdkclient.DeployResult, 1)
		go func() { done <- deploy(sha) }()
		ticker := time.NewTicker(100 * time.Millisecond)
	loop:
		for {
			select {
			case r := <-done:
				if r.Status != "failed" {
					t.Fatalf("wrong result: %+v", r)
				}
				break loop
			case <-ticker.C:
				read()
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		ticker.Stop()
		read()
		after, _ := d.compose(ctx, dir, "ps", "-q", "web")
		if string(before) != string(after) {
			t.Fatal("replaced healthy container")
		}
		saved, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
		if string(saved) != string(previous) {
			t.Fatal("changed configuration")
		}
		data, _ := os.ReadFile(marker)
		if string(data) != "keep" {
			t.Fatal("changed app data")
		}
	}
}
