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

func TestReplacementResumeNeverRedispatchesAndPersistsBeforeDelivery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux private receipt permissions")
	}
	for _, mode := range []string{"active-terminal", "dead-terminal", "invalid-receipt", "success", "failed", "lost-ack"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0700)
			state := "active"
			if mode == "dead-terminal" {
				state = "inactive"
			}
			os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho "+state+"\n"), 0700)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			var posts atomic.Int32
			var received sdkclient.DeployResult
			var p *Poller
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/agent/command-progress":
					fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_work","terminal":true,"status":"success"}}`)
					if mode != "success" && mode != "failed" && mode != "lost-ack" {
						stop()
					}
				case "/v1/agent/deploy-result":
					posts.Add(1)
					json.NewDecoder(r.Body).Decode(&received)
					persisted, err := p.journal.load()
					if err != nil || persisted.Result == nil {
						t.Error("result sent before durable save")
					}
					if mode == "lost-ack" {
						w.WriteHeader(503)
						stop()
					} else {
						w.WriteHeader(204)
					}
				default:
					t.Error("unexpected execution/control request: " + r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			p = testPoller(t, server.URL, dir, &receiptExecutor{})
			p.exec = &executor.Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			lock, err := p.journal.open()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			id := strings.Repeat("a", 32)
			workerDir := filepath.Join(p.journal.dir, "replacement-"+id)
			os.Mkdir(workerDir, 0700)
			request := map[string]any{"version": 1, "id": id, "command_id": "cmd_work", "state_dir": dir, "docker": "/usr/bin/docker", "config_sha256": strings.Repeat("b", 64), "payload": map[string]any{"deployment_id": "dpl_work"}}
			raw, _ := json.Marshal(request)
			sum := sha256.Sum256(raw)
			hash := hex.EncodeToString(sum[:])
			os.WriteFile(filepath.Join(workerDir, "request.json"), raw, 0600)
			if !strings.Contains(mode, "terminal") {
				status := "success"
				if mode == "failed" {
					status = "failed"
				}
				receipt := map[string]any{"version": 1, "id": id, "request_sha256": hash, "result": map[string]any{"command_id": "cmd_work", "deployment_id": "dpl_work", "status": status, "logs_tail": "original-only"}}
				if mode == "invalid-receipt" {
					receipt["request_sha256"] = strings.Repeat("c", 64)
				}
				raw, _ = json.Marshal(receipt)
				os.WriteFile(filepath.Join(workerDir, "result.json"), raw, 0600)
			}
			p.active = &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: "cmd_work", ControlToken: "private-token", ProgressProtocol: sdkclient.DeploymentProgressProtocol, Replacement: &executor.ReplacementWork{ID: id, CommandID: "cmd_work", DeploymentID: "dpl_work", RequestSHA256: hash}, Preparation: &executor.PreparationRecovery{Version: 1, Phase: "replacing", DeploymentID: "dpl_work", Files: []executor.PreparationFile{{Name: "compose.yaml"}, {Name: ".env"}, {Name: "startup.json"}}}}
			if err = p.journal.save(p.active); err != nil {
				t.Fatal(err)
			}
			if err = p.resumeRecord(ctx); err != nil {
				t.Fatal(err)
			}
			saved, err := p.journal.load()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "success" || mode == "failed" {
				if posts.Load() != 1 || saved != nil || received.Status != mode || received.ControlToken != "private-token" || received.LogsTail != "original-only" {
					t.Fatalf("wrong delivery %+v %+v", saved, received)
				}
				if _, err = os.Stat(workerDir); !os.IsNotExist(err) {
					t.Fatal("acknowledged work not removed")
				}
			} else if mode == "lost-ack" {
				if posts.Load() != 1 || saved == nil || saved.Result == nil {
					t.Fatal("lost receipt")
				}
				if _, err = os.Stat(workerDir); err != nil {
					t.Fatal("removed before ACK")
				}
			} else {
				if posts.Load() != 0 || saved == nil || saved.Result != nil {
					t.Fatal("uncertain work released")
				}
			}
		})
	}
}
func TestReplacementJournalRejectsForeignIdentityAndPhase(t *testing.T) {
	r := &commandRecord{CommandID: "cmd", Preparation: &executor.PreparationRecovery{Phase: "replacing", DeploymentID: "dpl_test"}, Replacement: &executor.ReplacementWork{ID: strings.Repeat("a", 32), CommandID: "cmd", DeploymentID: "dpl_test", RequestSHA256: strings.Repeat("b", 64)}}
	if err := validateReplacementRecord(r); err != nil {
		t.Fatal(err)
	}
	r.Preparation.Phase = "ready"
	if validateReplacementRecord(r) == nil {
		t.Fatal("preparation authorized replacement")
	}
	r.Preparation.Phase = "replacing"
	r.Replacement.CommandID = "foreign"
	if validateReplacementRecord(r) == nil {
		t.Fatal("foreign work accepted")
	}
}
