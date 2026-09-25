package poll

import (
	"context"
	"encoding/json"
	"errors"
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
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestDomainRecoveryPrecedesPrivacyGateAndFreshPolling(t *testing.T) {
	var accepted atomic.Bool
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/deploy-result" {
			var result sdkclient.DeployResult
			if json.NewDecoder(r.Body).Decode(&result) != nil || result.ControlToken != "domain-control" || result.DomainHandover == nil || result.DomainHandover.Status != "unchanged" {
				t.Error("invalid domain recovery receipt")
			}
			accepted.Store(true)
			w.WriteHeader(204)
			return
		}
		polls.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	d := executor.NewDocker(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, err := New(&config.Config{AgentID: "agt_fixture", AgentSecret: "fixture", ControlPlaneURL: server.URL}, d, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	record := &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: "cmd_aaaaaaaaaaaaaaaa", Kind: sdkclient.CommandUpdateRoutes,
		ControlToken: "domain-control", ProgressProtocol: sdkclient.DeploymentProgressProtocol, Step: "preparing",
		DomainHandover: &executor.DomainHandoverIdentity{DeploymentID: "dpl_aaaaaaaaaaaaaaaa", Before: "before.example.test", After: "after.example.test"}}
	if err := p.journal.save(record); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	gateErr := errors.New("privacy gate refused by fixture")
	p.BeforeCommands = func(context.Context) error {
		if !accepted.Load() {
			t.Error("privacy gate ran before domain recovery acknowledgement")
		}
		if saved, err := p.journal.load(); err != nil || saved != nil {
			t.Error("acknowledged operation still pending")
		}
		if other, err := p.journal.open(); err == nil {
			other.Close()
			t.Error("privacy gate did not hold the operation lock")
		}
		return gateErr
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := p.Run(ctx); !errors.Is(err, gateErr) {
		t.Fatal("privacy gate outcome lost", err)
	}
	if polls.Load() != 0 {
		t.Fatal("fresh commands or heartbeat escaped an unverified privacy gate")
	}
}

type receiptExecutor struct {
	calls  atomic.Int32
	status string
}

func (e *receiptExecutor) Execute(ctx context.Context, c *sdkclient.PollCommand) sdkclient.DeployResult {
	e.calls.Add(1)
	if c.Kind == sdkclient.CommandHostFailoverFence {
		var p sdkclient.HostFailoverFencePayload
		if err := c.As(&p); err != nil {
			return sdkclient.DeployResult{CommandID: c.ID, Status: "failed", Error: err.Error()}
		}
		return sdkclient.DeployResult{CommandID: c.ID, Status: "success", DeploymentID: p.DeploymentID,
			HostFailoverFence: &sdkclient.HostFailoverFenceResult{DeploymentID: p.DeploymentID,
				Hostname: p.Hostname, Epoch: p.Epoch, CutoverID: p.CutoverID,
				RoutesRemoved: true, ContainersStopped: true}}
	}
	return sdkclient.DeployResult{CommandID: c.ID, Status: e.status, PreparationRestored: e.status == "cancelled", LogsTail: "saved exactly", AdminCredentials: map[string]string{"password": "private-fixture"}}
}

func TestInterruptedFenceReplaysOnlySavedReviewedPayload(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	ctx2, stop2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop2()
	var accepted atomic.Bool
	var polls, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/report":
			w.WriteHeader(204)
		case "/v1/agent/command-progress":
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_fence","terminal":false,"status":"in_progress"}}`)
		case "/v1/agent/poll":
			polls.Add(1)
			stop2()
			w.WriteHeader(204)
		case "/v1/agent/deploy-result":
			var result sdkclient.DeployResult
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Error(err)
			}
			if result.ControlToken != "fence-control" || result.HostFailoverFence == nil ||
				!result.HostFailoverFence.RoutesRemoved || !result.HostFailoverFence.ContainersStopped ||
				result.HostFailoverFence.Epoch != 2 {
				t.Errorf("invalid fence receipt: %+v", result)
			}
			posts.Add(1)
			if !accepted.Load() {
				w.WriteHeader(503)
				stop()
				return
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	e := &receiptExecutor{}
	p := testPoller(t, server.URL, dir, e)
	payload := json.RawMessage(`{"deployment_id":"dpl_aaaaaaaaaaaaaaaa","hostname":"site-abcdef.imprezaapps.com","epoch":2,"cutover_id":"fov_aaaaaaaaaaaaaaaaaaaaaaaa"}`)
	record := &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL,
		CommandID: "cmd_fence", Kind: sdkclient.CommandHostFailoverFence,
		ControlToken: "fence-control", ProgressProtocol: sdkclient.DeploymentProgressProtocol,
		Step: "preparing", Payload: payload}
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.journal.save(record); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	lock.Close()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := p.journal.load()
	if err != nil || saved == nil || saved.Result == nil || e.calls.Load() != 1 || posts.Load() != 1 || polls.Load() != 0 {
		t.Fatalf("fence not durably retried: saved=%+v err=%v calls=%d posts=%d polls=%d", saved, err, e.calls.Load(), posts.Load(), polls.Load())
	}
	accepted.Store(true)
	p2 := testPoller(t, server.URL, dir, e)
	if err := p2.Run(ctx2); err != nil {
		t.Fatal(err)
	}
	saved, err = p2.journal.load()
	if err != nil || saved != nil || e.calls.Load() != 1 || posts.Load() != 2 || polls.Load() != 1 {
		t.Fatalf("fence ACK replay mismatch: saved=%+v err=%v calls=%d posts=%d polls=%d", saved, err, e.calls.Load(), posts.Load(), polls.Load())
	}
}
func testPoller(t *testing.T, url, dir string, e *receiptExecutor) *Poller {
	t.Helper()
	p, err := New(&config.Config{AgentID: "agt_fixture", AgentSecret: "fixture", ControlPlaneURL: url, HeartbeatSeconds: 60, BackoffMinSeconds: 1, BackoffMaxSeconds: 1}, e, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p.journal = &commandJournal{dir: filepath.Join(dir, "operations")}
	return p
}
func TestSavedResultsSurviveRestartWithoutExecution(t *testing.T) {
	for _, status := range []string{"success", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			ctx2, stop2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop2()
			var polls, posts atomic.Int32
			var second atomic.Bool
			received := make(chan sdkclient.DeployResult, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/agent/report":
					w.WriteHeader(204)
				case "/v1/agent/poll":
					if second.Load() {
						stop2()
						w.WriteHeader(204)
						return
					}
					if polls.Add(1) != 1 {
						t.Error("unexpected redispatch")
						w.WriteHeader(204)
						return
					}
					fmt.Fprint(w, `{"success":true,"data":{"id":"cmd_saved","kind":"deploy","control_token":"private-control","progress_protocol":"deploy-progress-v1","payload":{}}}`)
				case "/v1/agent/command-progress":
					fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_saved","terminal":false,"status":"in_progress"}}`)
				case "/v1/agent/deploy-result":
					var result sdkclient.DeployResult
					if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
						t.Error(err)
					}
					received <- result
					posts.Add(1)
					if !second.Load() {
						w.WriteHeader(503)
						stop()
						return
					}
					w.WriteHeader(204)
				default:
					t.Error(r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			e := &receiptExecutor{status: status}
			p := testPoller(t, server.URL, dir, e)
			if err := p.Run(ctx); err != nil {
				t.Fatal(err)
			}
			saved, err := p.journal.load()
			if err != nil || saved == nil || saved.Result == nil {
				t.Fatalf("receipt missing: %v", err)
			}
			info, err := os.Stat(filepath.Join(p.journal.dir, "pending.json"))
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("private receipt permissions")
			}
			second.Store(true)
			p2 := testPoller(t, server.URL, dir, e)
			if err := p2.Run(ctx2); err != nil {
				t.Fatal(err)
			}
			if posts.Load() != 2 || e.calls.Load() != 1 {
				t.Fatalf("posts=%d executions=%d", posts.Load(), e.calls.Load())
			}
			first, last := <-received, <-received
			if !reflect.DeepEqual(first, last) {
				t.Fatal("saved result changed across restart")
			}
			if row, err := p2.journal.load(); row != nil || err != nil {
				t.Fatalf("acknowledged receipt not removed: %v", err)
			}
		})
	}
}
func TestLostPollResponseRequiresReconciliation(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/report":
			w.WriteHeader(204)
		case "/v1/agent/poll":
			fmt.Fprint(w, `{"success":true,"data":{"id":"cmd_lost","kind":"deploy","control_token":"token","progress_protocol":"deploy-progress-v1","resume_only":true,"payload":{}}}`)
		case "/v1/agent/command-progress":
			var req sdkclient.DeploymentProgress
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Step != "interrupted" {
				t.Error("missing interrupted report")
			}
			reports.Add(1)
			fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_lost","terminal":false,"status":"in_progress"}}`)
			stop()
		default:
			t.Error("unexpected action " + r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	e := &receiptExecutor{}
	p := testPoller(t, server.URL, t.TempDir(), e)
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if e.calls.Load() != 0 || reports.Load() != 1 {
		t.Fatal("lost poll response was executed")
	}
	row, err := p.journal.load()
	if err != nil || row == nil || row.Result != nil {
		t.Fatal("uncertain operation must remain recorded")
	}
}
func TestJournalLockAndInvalidRecords(t *testing.T) {
	j := &commandJournal{dir: filepath.Join(t.TempDir(), "operations")}
	f, err := j.open()
	if err != nil {
		t.Fatal(err)
	}
	if other, err := j.open(); err == nil {
		other.Close()
		t.Fatal("second process lock accepted")
	}
	f.Close()
	f, err = j.open()
	if err != nil {
		t.Fatal("lock not released", err)
	}
	defer f.Close()
	record := &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: "http://fixture", CommandID: "cmd_test", ControlToken: "secret", ProgressProtocol: sdkclient.DeploymentProgressProtocol}
	if err := j.save(record); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"version":1`, `{}`, `{"version":100}`} {
		if err := os.WriteFile(filepath.Join(j.dir, "pending.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := j.load(); err == nil {
			t.Fatal("invalid record accepted")
		}
	}
	if err := j.save(record); err != nil {
		t.Fatal(err)
	}
	record.Result = &sdkclient.DeployResult{CommandID: "cmd_other", ControlToken: "secret", Status: "success"}
	if err := j.save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := j.load(); err == nil {
		t.Fatal("mismatched receipt accepted")
	}
}
func TestChangedAgentIdentityBlocksReplay(t *testing.T) {
	e := &receiptExecutor{}
	p := testPoller(t, "http://127.0.0.1:1", t.TempDir(), e)
	f, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	err = p.journal.save(&commandRecord{Version: 1, AgentID: "agt_other", ControlPlaneURL: p.cfg.ControlPlaneURL, CommandID: "cmd_test", ControlToken: "token", ProgressProtocol: sdkclient.DeploymentProgressProtocol})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("foreign agent result replay allowed")
	}
	if e.calls.Load() != 0 {
		t.Fatal("unexpected execution")
	}
}

func TestUnstartedRecoveryVerifiesControlAndPersistsOutcome(t *testing.T) {
	for _, mode := range []string{"failed", "cancelled", "wrong-command", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			var posts, controls, interrupts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/agent/poll":
					stop()
					w.WriteHeader(204)
				case "/v1/agent/report":
					w.WriteHeader(204)
				case "/v1/agent/command-progress":
					var b sdkclient.DeploymentProgress
					json.NewDecoder(r.Body).Decode(&b)
					if b.Step == "interrupted" && interrupts.Add(1) > 1 {
						stop()
					}
					fmt.Fprint(w, "{\"success\":true,\"data\":{\"command_id\":\"cmd_unstarted\",\"terminal\":false,\"status\":\"in_progress\"}}")
				case "/v1/agent/command-control":
					controls.Add(1)
					var b sdkclient.DeploymentControl
					json.NewDecoder(r.Body).Decode(&b)
					if b.CommandID != "cmd_unstarted" || b.ControlToken != "private" || b.Phase != "preparing" {
						t.Error("wrong control identity")
					}
					id, phase := "cmd_unstarted", "preparing"
					if mode == "wrong-command" {
						id = "cmd_other"
					}
					if mode == "replacement" {
						phase = "replacing"
					}
					fmt.Fprintf(w, "{\"success\":true,\"data\":{\"command_id\":%q,\"phase\":%q,\"cancel_requested\":%t}}", id, phase, mode == "cancelled")
				case "/v1/agent/deploy-result":
					posts.Add(1)
					var b sdkclient.DeployResult
					json.NewDecoder(r.Body).Decode(&b)
					if b.Status != mode || !b.PreparationRestored || b.CommandID != "cmd_unstarted" || b.ControlToken != "private" {
						t.Error("incorrect reconciled result")
					}
					w.WriteHeader(204)
				default:
					t.Error("unexpected execution or poll: " + r.URL.Path)
					w.WriteHeader(500)
					stop()
				}
			}))
			defer server.Close()
			cfg := &config.Config{AgentID: "agt_fixture", AgentSecret: "fixture", ControlPlaneURL: server.URL, HeartbeatSeconds: 60, BackoffMinSeconds: 1, BackoffMaxSeconds: 1}
			d := &executor.Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			p, err := New(cfg, d, "test", d.Log)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := p.journal.open()
			if err != nil {
				t.Fatal(err)
			}
			err = p.journal.save(&commandRecord{Version: 1, AgentID: cfg.AgentID, ControlPlaneURL: cfg.ControlPlaneURL, CommandID: "cmd_unstarted", ControlToken: "private", ProgressProtocol: sdkclient.DeploymentProgressProtocol, Preparation: &executor.PreparationRecovery{Version: 1, Phase: "unstarted"}})
			lock.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Run(ctx); err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if mode == "wrong-command" || mode == "replacement" {
				want = 0
			}
			if posts.Load() != want || controls.Load() != 1 {
				t.Fatalf("posts=%d controls=%d", posts.Load(), controls.Load())
			}
			record, err := p.journal.load()
			if err != nil {
				t.Fatal(err)
			}
			if (record == nil) != (want == 1) {
				t.Fatal("uncertain journal cleared or acknowledged result retained")
			}
			if _, err := os.Stat(filepath.Join(d.StateDir, "apps")); !os.IsNotExist(err) {
				t.Fatal("unstarted recovery modified app state")
			}
		})
	}
}
func TestPreparationPersistenceFailureIsSticky(t *testing.T) {
	p := testPoller(t, "http://127.0.0.1:1", t.TempDir(), &receiptExecutor{})
	state := &executor.PreparationRecovery{Version: 1, Phase: "unstarted"}
	original := &commandRecord{CommandID: "cmd_test", ControlToken: "private", Preparation: state}
	p.active = original
	cmd := &sdkclient.PollCommand{ID: "cmd_test", ControlToken: "private"}
	if err := p.savePreparation(cmd, state); err == nil || p.journalErr == nil {
		t.Fatal("missing directory did not stop persistence")
	}
	if p.active != original {
		t.Fatal("unpersisted checkpoint became current")
	}
	os.MkdirAll(p.journal.dir, 0700)
	if err := p.savePreparation(cmd, state); err == nil {
		t.Fatal("persistence failure was forgotten")
	}
}
