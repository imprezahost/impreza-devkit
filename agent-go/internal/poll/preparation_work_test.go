package poll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisedWorkBlocksRedispatchAndChecksServerPhase(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux durable worker and filesystem checks")
	}
	for _, mode := range []string{"active-terminal-server", "aborted-terminal-server", "failed-terminal-server", "invalid-receipt", "replacement", "success", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0700)
			for name, script := range map[string]string{"systemctl": "echo active", "docker": "printf '%s\n' " + strings.Repeat("a", 64)} {
				os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0700)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			ctx, stop := context.WithTimeout(context.Background(), 4*time.Second)
			defer stop()
			var posts, controls, progress atomic.Int32
			var received sdkclient.DeployResult
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/agent/command-progress":
					count := progress.Add(1)
					terminal := strings.Contains(mode, "terminal-server")
					fmt.Fprintf(w, `{"success":true,"data":{"command_id":"cmd_work","terminal":%t,"status":"in_progress"}}`, terminal)
					if !strings.Contains(mode, "success") && mode != "cancelled" && (mode != "replacement" || count >= 3) {
						stop()
					}
				case "/v1/agent/command-control":
					controls.Add(1)
					phase := "preparing"
					if mode == "replacement" {
						phase = "replacing"
					}
					fmt.Fprintf(w, `{"success":true,"data":{"command_id":"cmd_work","phase":%q,"cancel_requested":%t}}`, phase, mode == "cancelled")
				case "/v1/agent/deploy-result":
					posts.Add(1)
					json.NewDecoder(r.Body).Decode(&received)
					w.WriteHeader(204)
				default:
					t.Error("unexpected action: " + r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			p := testPoller(t, server.URL, dir, &receiptExecutor{})
			p.exec = &executor.Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			lock, err := p.journal.open()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			workID := strings.Repeat("b", 32)
			workerDir := filepath.Join(p.journal.dir, "preparation-"+workID)
			os.Mkdir(workerDir, 0700)
			request := map[string]any{"version": 1, "id": workID, "command_id": "cmd_work", "deployment_id": "dpl_work", "state_dir": dir, "step": "build"}
			raw, _ := json.Marshal(request)
			sum := sha256.Sum256(raw)
			hash := hex.EncodeToString(sum[:])
			os.WriteFile(filepath.Join(workerDir, "request.json"), raw, 0600)
			if mode != "active-terminal-server" {
				receipt := map[string]any{"version": 1, "id": workID, "request_sha256": hash, "completed": true, "success": mode != "failed-terminal-server"}
				if mode == "invalid-receipt" {
					receipt["id"] = strings.Repeat("c", 32)
				}
				raw, _ = json.Marshal(receipt)
				os.WriteFile(filepath.Join(workerDir, "result.json"), raw, 0600)
			}
			app := filepath.Join(dir, "apps", "dpl_work")
			os.MkdirAll(app, 0700)
			os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("new"), 0600)
			p.active = &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: "cmd_work", ControlToken: "private-token", ProgressProtocol: sdkclient.DeploymentProgressProtocol, Preparation: &executor.PreparationRecovery{Version: 1, Phase: "busy", DeploymentID: "dpl_work", Files: []executor.PreparationFile{{Name: "compose.yaml", Data: []byte("old"), Mode: 0600, Exists: true}, {Name: ".env"}, {Name: "startup.json"}}, Containers: []string{strings.Repeat("a", 64)}, Work: &executor.PreparationWork{ID: workID, Step: "build", CommandID: "cmd_work", RequestSHA256: hash}}}
			if mode == "aborted-terminal-server" {
				p.active.Preparation.Phase = "aborted"
			}
			if err = p.journal.save(p.active); err != nil {
				t.Fatal(err)
			}
			if err = p.resumeRecord(ctx); err != nil {
				t.Fatal(err)
			}
			config, _ := os.ReadFile(filepath.Join(app, "compose.yaml"))
			saved, err := p.journal.load()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "success" || mode == "cancelled" {
				if posts.Load() != 1 || controls.Load() != 1 || saved != nil || string(config) != "old" {
					t.Fatalf("reconciliation failed: %d %d %v %s", posts.Load(), controls.Load(), saved, config)
				}
				want := "failed"
				if mode == "cancelled" {
					want = "cancelled"
				}
				if received.Status != want || !received.PreparationRestored {
					t.Fatalf("wrong result %+v", received)
				}
			} else {
				if posts.Load() != 0 || saved == nil || saved.Result != nil || string(config) != "new" {
					t.Fatal("unproven operation released or state changed")
				}
				if mode != "replacement" && controls.Load() != 0 {
					t.Fatal("server phase requested before work completed")
				}
			}
		})
	}
}

func TestAbortedPreparationTerminalServerPreservesJournal(t *testing.T) {
	dir := t.TempDir()
	var progress atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/agent/command-progress" {
			t.Errorf("unexpected action %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		count := progress.Add(1)
		fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_reboot","terminal":true,"status":"failed"}}`)
		if count >= 2 {
			cancel()
		}
	}))
	defer server.Close()
	p := testPoller(t, server.URL, dir, &receiptExecutor{})
	p.exec = &executor.Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	p.active = &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: "cmd_reboot", ControlToken: "fixture", ProgressProtocol: sdkclient.DeploymentProgressProtocol, Preparation: &executor.PreparationRecovery{Version: 1, Phase: "aborted", DeploymentID: "dpl_reboot", Files: []executor.PreparationFile{{Name: "compose.yaml"}, {Name: ".env"}, {Name: "startup.json"}}, Work: &executor.PreparationWork{ID: strings.Repeat("b", 32), Step: "build", CommandID: "cmd_reboot", RequestSHA256: strings.Repeat("c", 64)}}}
	if err := p.journal.save(p.active); err != nil {
		t.Fatal(err)
	}
	if err := p.resumeRecord(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := p.journal.load()
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || p.active == nil || saved.Preparation.Phase != "aborted" || saved.Result != nil {
		t.Fatal("terminal server discarded unreconciled reboot state")
	}
}
