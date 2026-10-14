package poll

import (
	"context"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type backupQueueExecutor struct {
	receiptExecutor
	clean atomic.Int32
	fail  bool
}

func (e *backupQueueExecutor) Execute(ctx context.Context, c *sdkclient.PollCommand) sdkclient.DeployResult {
	e.calls.Add(1)
	id := "dpl_aaaaaaaaaaaaaaaa"
	if c.ID == "cmd_legacy" {
		id = "bkpjob_bbbbbbbbbbbbbbbb"
	}
	return sdkclient.DeployResult{CommandID: c.ID, DeploymentID: id, Status: "success"}
}
func (e *backupQueueExecutor) CleanupAcknowledgedResult(ctx context.Context, k sdkclient.CommandKind, r sdkclient.DeployResult) error {
	e.clean.Add(1)
	if e.fail {
		return errors.New("permanent foreign project")
	}
	return nil
}
func TestBackupPollContinuesAfterPermanentCleanupFailure(t *testing.T) {
	var polls, acks atomic.Int32
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/deploy-result":
			acks.Add(1)
			w.WriteHeader(204)
		case "/v1/agent/poll":
			if polls.Add(1) == 1 {
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": sdkclient.PollCommand{ID: "cmd_new", Kind: sdkclient.CommandDeploy, Payload: json.RawMessage(`{}`)}})
			} else {
				stop()
				w.WriteHeader(204)
			}
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	e := &backupQueueExecutor{fail: true}
	p := testPoller(t, server.URL, t.TempDir(), &e.receiptExecutor)
	p.exec = e
	r := sdkclient.DeployResult{CommandID: "cmd_old", ControlToken: "fixture", DeploymentID: "bkpjob_aaaaaaaaaaaaaaaa", Status: "failed"}
	p.active = &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: r.CommandID, Kind: sdkclient.CommandDeploy, ControlToken: r.ControlToken, ProgressProtocol: sdkclient.DeploymentProgressProtocol, Result: &r}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = p.journal.save(p.active); err != nil {
		t.Fatal(err)
	}
	if err = p.pollLoop(ctx); err != nil {
		t.Fatal("cleanup holds fresh polling", err)
	}
	if polls.Load() < 2 || e.calls.Load() != 1 || acks.Load() != 2 || p.active != nil {
		t.Fatalf("queue stuck polls=%d execute=%d ACK=%d", polls.Load(), e.calls.Load(), acks.Load())
	}
}
func TestUncontrolledSendFailureCannotClean(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "ACK"}[accepted], func(t *testing.T) {
			ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
			defer stop()
			var polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/agent/poll":
					if polls.Add(1) == 1 {
						json.NewEncoder(w).Encode(map[string]any{"success": true, "data": sdkclient.PollCommand{ID: "cmd_legacy", Kind: sdkclient.CommandDeploy, Payload: json.RawMessage(`{}`)}})
					} else {
						stop()
						w.WriteHeader(204)
					}
				case "/v1/agent/deploy-result":
					if accepted {
						w.WriteHeader(204)
					} else {
						w.WriteHeader(503)
					}
				default:
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			e := &backupQueueExecutor{}
			p := testPoller(t, server.URL, t.TempDir(), &e.receiptExecutor)
			p.exec = e
			lock, err := p.journal.open()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err = p.pollLoop(ctx); err != nil {
				t.Fatal(err)
			}
			if accepted {
				p.nextBackupCleanup = time.Time{}
				p.reconcileAcknowledgedBackups(t.Context())
				if e.clean.Load() != 1 {
					t.Fatal("ACK did not authorize exactly one cleanup")
				}
			}
			if !accepted {
				dir, err := p.cleanupDirectory()
				if err != nil {
					t.Fatal(err)
				}
				files, err := os.ReadDir(dir)
				if err != nil || len(files) != 0 {
					t.Fatal("failed send authorized deferred cleanup", err)
				}
			}
			if !accepted && e.clean.Load() != 0 {
				t.Fatal("cleanup ran without ACK")
			}
		})
	}
}

func TestBackupCleanupIdentityStorageAndRestart(t *testing.T) {
	e := &backupQueueExecutor{fail: true}
	p := testPoller(t, "http://fixture.invalid", t.TempDir(), &e.receiptExecutor)
	p.exec = e
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	result := sdkclient.DeployResult{CommandID: "cmd_secret", DeploymentID: "bkpjob_aaaaaaaaaaaaaaaa", Status: "failed", ControlToken: "secret-token", LogsTail: "secret-logs", AdminCredentials: map[string]string{"password": "secret-password"}}
	if err = p.queueAcknowledgedBackup(sdkclient.CommandDeploy, result); err != nil {
		t.Fatal(err)
	}
	dir, err := p.cleanupDirectory()
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("no durable cleanup", err)
	}
	name := filepath.Join(dir, files[0].Name())
	raw, err := os.ReadFile(name)
	if err != nil || strings.Contains(string(raw), "secret-token") || strings.Contains(string(raw), "secret-logs") || strings.Contains(string(raw), "secret-password") {
		t.Fatal("cleanup stored secrets", err)
	}
	var r backupCleanupRecord
	if json.Unmarshal(raw, &r) != nil {
		t.Fatal("invalid record")
	}
	r.AgentID = "foreign"
	if err = saveBackupCleanup(dir, r); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(name); err != nil {
		t.Fatal(err)
	}
	p.reconcileAcknowledgedBackups(t.Context())
	if e.clean.Load() != 0 {
		t.Fatal("foreign authority used")
	}
	r.AgentID = p.cfg.AgentID
	r.Attempts = 2
	r.NextAttempt = time.Time{}
	if err = saveBackupCleanup(dir, r); err != nil {
		t.Fatal(err)
	}
	p.nextBackupCleanup = time.Time{}
	p.reconcileAcknowledgedBackups(t.Context())
	if e.clean.Load() != 1 {
		t.Fatal("restart did not resume remaining attempt")
	}
	if err = p.queueAcknowledgedBackup(sdkclient.CommandDeploy, result); err != nil {
		t.Fatal(err)
	}
	p.nextBackupCleanup = time.Time{}
	p.reconcileAcknowledgedBackups(t.Context())
	if e.clean.Load() != 1 {
		t.Fatal("ACK replay reset exhausted budget")
	}
	bad := result
	bad.DeploymentID = "bkpjob_bbbbbbbbbbbbbbbb"
	if err = p.queueAcknowledgedBackup(sdkclient.CommandDeploy, bad); err == nil {
		t.Fatal("changed identity accepted")
	}
	if err = os.Rename(dir, dir+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dir, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = p.queueAcknowledgedBackup(sdkclient.CommandDeploy, result); err == nil {
		t.Fatal("unwritable queue discarded receipt")
	}
	raw, err = os.ReadFile(dir)
	if err != nil || string(raw) != "sentinel" {
		t.Fatal("foreign storage altered")
	}
}
