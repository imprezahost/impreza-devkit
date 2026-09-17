package poll

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollReportsUnsupportedAndContinues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kinds := []sdkclient.CommandKind{sdkclient.CommandAgentUpgrade, sdkclient.CommandUpdate, "future_command", sdkclient.CommandDeploy}
	results := make(chan sdkclient.DeployResult, len(kinds))
	var next atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Agent-Id") != "agt_fixture" || r.Header.Get("X-Agent-Secret") != "fixture-only" {
			http.Error(w, "wrong authentication", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/poll":
			var request sdkclient.PollRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Capabilities) != 9 || request.Capabilities[0] != "startup-health-v1" || request.Capabilities[1] != "deploy-cancel-v1" || request.Capabilities[2] != "build-secrets-v1" || request.Capabilities[3] != "compose-source-files-v1" || request.Capabilities[4] != sdkclient.ServiceBindingProtocol || request.Capabilities[5] != sdkclient.ServiceBindingRetirementProtocol || request.Capabilities[6] != sdkclient.ServiceBindingGenerationProtocol || request.Capabilities[7] != sdkclient.ServiceBindingGenerationRetirementProtocol || request.Capabilities[8] != sdkclient.DeploymentProgressProtocol {
				t.Error("missing startup capability")
				http.Error(w, "capability missing", 400)
				return
			}
			index := int(next.Add(1)) - 1
			if index >= len(kinds) {
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Millisecond):
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			command := sdkclient.PollCommand{ID: fmt.Sprintf("cmd_%d", index), Kind: kinds[index], Payload: json.RawMessage(`{"secret":"must-not-echo"}`)}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": command})
		case "/v1/agent/deploy-result":
			var result sdkclient.DeployResult
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				http.Error(w, "bad result", 400)
				return
			}
			select {
			case results <- result:
			case <-ctx.Done():
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/agent/report":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	marker := filepath.Join(root, "state")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(&config.Config{AgentID: "agt_fixture", AgentSecret: "fixture-only", ControlPlaneURL: server.URL, HeartbeatSeconds: 1, BackoffMinSeconds: 1, BackoffMaxSeconds: 1}, &executor.Docker{StateDir: root, Log: log}, "unreleased", log)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	// Stop the loop before closing the HTTP fixture even when an assertion fails.
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("poller failed to stop")
		}
	}()
	for i := range kinds {
		select {
		case result := <-results:
			if result.CommandID != fmt.Sprintf("cmd_%d", i) || result.Status != "failed" {
				t.Fatalf("incorrect report: %+v", result)
			}
			if i < 3 && !strings.Contains(result.Error, "Unsupported command") {
				t.Fatalf("missing refusal: %+v", result)
			}
			if i == 3 && (strings.Contains(result.Error, "Unsupported command") || !strings.Contains(result.Error, "missing deployment_id")) {
				t.Fatalf("subsequent supported command not dispatched: %+v", result)
			}
			if strings.Contains(result.Error, "must-not-echo") || result.LogsTail != "" {
				t.Fatalf("payload leaked: %+v", result)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for reported result")
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "preserve" {
		t.Fatal("state changed")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 || entries[0].Name() != "operations" || entries[1].Name() != "state" {
		t.Fatal("unexpected filesystem changes")
	}
}
