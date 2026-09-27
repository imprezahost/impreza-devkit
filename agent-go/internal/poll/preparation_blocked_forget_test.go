package poll

// A Blocked deployment (onion, data_dir) interrupted
// mid-pull reconciles its worker as ready or aborted, and with the server
// already terminal the resume path clears the journal. It did so without
// forgetting the worker files: operations/preparation-<id>/ stayed forever,
// with request.json (the proxy variables it carries can hold credentials)
// and the pull's 4 KB output tail. They go before the journal now.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestBlockedTerminalClearForgetsWorkerFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux durable worker and filesystem checks")
	}
	for _, phase := range []string{"ready", "aborted"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/v1/agent/command-progress" {
					t.Errorf("unexpected action %s", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_blocked","terminal":true,"status":"failed"}}`)
			}))
			defer server.Close()
			p := testPoller(t, server.URL, dir, &receiptExecutor{})
			p.exec = &executor.Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			lock, err := p.journal.open()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			workID := strings.Repeat("e", 32)
			workerDir := filepath.Join(p.journal.dir, "preparation-"+workID)
			if err := os.Mkdir(workerDir, 0o700); err != nil {
				t.Fatal(err)
			}
			request, _ := json.Marshal(map[string]any{"version": 1, "id": workID, "command_id": "cmd_blocked", "deployment_id": "dpl_blocked", "state_dir": dir, "step": "pull",
				"env": []string{"HTTPS_PROXY=http://proxy-user:fixture-only@proxy.invalid:3128"}})
			sum := sha256.Sum256(request)
			hash := hex.EncodeToString(sum[:])
			receipt, _ := json.Marshal(map[string]any{"version": 1, "id": workID, "request_sha256": hash, "completed": true, "success": phase == "ready", "output": "pull output tail"})
			for name, raw := range map[string][]byte{"request.json": request, "started": nil, "result.json": receipt} {
				if err := os.WriteFile(filepath.Join(workerDir, name), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p.active = &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: "cmd_blocked", ControlToken: "fixture", ProgressProtocol: sdkclient.DeploymentProgressProtocol,
				Preparation: &executor.PreparationRecovery{Version: 1, Phase: phase, Blocked: true, DeploymentID: "dpl_blocked",
					Files: []executor.PreparationFile{{Name: "compose.yaml"}, {Name: ".env"}, {Name: "startup.json"}},
					Work:  &executor.PreparationWork{ID: workID, Step: "pull", CommandID: "cmd_blocked", RequestSHA256: hash}}}
			if err := p.journal.save(p.active); err != nil {
				t.Fatal(err)
			}
			if err := p.resumeRecord(ctx); err != nil {
				t.Fatal(err)
			}
			if saved, err := p.journal.load(); err != nil || saved != nil || p.active != nil {
				t.Fatalf("the terminal Blocked operation was not released: %+v %v", saved, err)
			}
			if _, err := os.Stat(workerDir); !os.IsNotExist(err) {
				entries, _ := os.ReadDir(workerDir)
				t.Fatalf("the Blocked worker files outlived the journal (%d entries left in %s)", len(entries), filepath.Base(workerDir))
			}
		})
	}
}
