package poll

import (
	"context"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"net/http"
	"net/http/httptest"
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
func TestCleanupFollowsACKAndRetainsFailedCleanupReceipt(t *testing.T) {
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
	if err = p.sendSavedResult(t.Context()); err == nil {
		t.Fatal("cleanup failure discarded receipt")
	}
	saved, err := p.journal.load()
	if err != nil || !reflect.DeepEqual(saved, record) || p.active == nil {
		t.Fatal("cleanup failure lost retry", err)
	}
	e.cleanupErr = nil
	if err = p.sendSavedResult(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved, err = p.journal.load()
	if err != nil || saved != nil || p.active != nil || e.callsCleanup != 2 || len(received) != 3 {
		t.Fatal("ACK cleanup/retry mismatch", err)
	}
	for _, got := range received {
		if !reflect.DeepEqual(got, want) {
			t.Fatal("receipt changed during retry")
		}
	}
}
