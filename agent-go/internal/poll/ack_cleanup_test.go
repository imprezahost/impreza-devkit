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
	"reflect"
	"testing"
	"time"
)

type ackCleanupExecutor struct {
	receiptExecutor
	callsCleanup int
	cleanupErr   error
	want         sdkclient.DeployResult
	t            *testing.T
}

func (e *ackCleanupExecutor) CleanupAcknowledgedResult(ctx context.Context, kind sdkclient.CommandKind, got sdkclient.DeployResult) error {
	e.callsCleanup++
	if kind != sdkclient.CommandDeploy || !reflect.DeepEqual(got, e.want) {
		e.t.Fatal("cleanup received a different receipt")
	}
	return e.cleanupErr
}
func TestCleanupFollowsACKAndRetainsBoundedReconciliation(t *testing.T) {
	var received []sdkclient.DeployResult
	status := 503
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/command-progress" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":true,"data":{"command_id":"cmd_ack_cleanup","terminal":false}}`))
			return
		}
		if r.URL.Path != "/v1/agent/deploy-result" {
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		var got sdkclient.DeployResult
		if json.NewDecoder(r.Body).Decode(&got) != nil {
			t.Error("invalid receipt")
		}
		received = append(received, got)
		w.WriteHeader(status)
	}))
	defer server.Close()
	want := sdkclient.DeployResult{CommandID: "cmd_ack_cleanup", ControlToken: "fixture-control", DeploymentID: "bkpjob_aaaaaaaaaaaaaaaa", Status: "failed", Error: "preserved failure", LogsTail: "preserved logs"}
	e := &ackCleanupExecutor{want: want, t: t}
	p := testPoller(t, server.URL, t.TempDir(), &e.receiptExecutor)
	p.exec = e
	record := &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: want.CommandID, Kind: sdkclient.CommandDeploy, ControlToken: want.ControlToken, ProgressProtocol: sdkclient.DeploymentProgressProtocol, Result: &want}
	p.active = record
	lock, err := p.journal.open()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = p.journal.save(record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	if err = p.sendSavedResult(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if e.callsCleanup != 0 || p.active == nil {
		t.Fatal("cleanup ran without acknowledgement")
	}
	status = 204
	e.cleanupErr = errors.New("fixture storage fault")
	if err = p.sendSavedResult(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved, err := p.journal.load()
	if err != nil || saved != nil || p.active != nil {
		t.Fatal("ACK did not release operation", err)
	}
	e.want.ControlToken = ""
	e.want.Error = ""
	e.want.LogsTail = ""
	for i := 0; i < 3; i++ {
		p.nextBackupCleanup = time.Time{}
		dir, err := p.cleanupDirectory()
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var r backupCleanupRecord
			if json.Unmarshal(raw, &r) != nil {
				t.Fatal("invalid cleanup")
			}
			r.NextAttempt = time.Time{}
			if err = saveBackupCleanup(dir, r); err != nil {
				t.Fatal(err)
			}
		}
		p.reconcileAcknowledgedBackups(t.Context())
	}
	p.nextBackupCleanup = time.Time{}
	p.reconcileAcknowledgedBackups(t.Context())
	if e.callsCleanup != 3 || len(received) != 2 {
		t.Fatal("cleanup exceeded budget or resent ACK", e.callsCleanup, len(received))
	}
	for _, got := range received {
		if !reflect.DeepEqual(got, want) {
			t.Fatal("receipt changed during retry")
		}
	}
}
