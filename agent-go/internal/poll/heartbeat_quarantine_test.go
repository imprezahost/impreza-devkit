package poll

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// quarantinedExec is an executor holding a quarantined restore, as the
// Docker executor does after a startup reconciliation poisons a journal.
type quarantinedExec struct {
	executor.Echo
	reports []sdkclient.RestoreQuarantineReport
}

func (q quarantinedExec) CollectRestoreQuarantine() []sdkclient.RestoreQuarantineReport {
	return q.reports
}

func heartbeatBody(t *testing.T, exec executor.Executor) map[string]json.RawMessage {
	t.Helper()
	bodies := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/report" {
			http.NotFound(w, r)
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad report", http.StatusBadRequest)
			return
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(&config.Config{AgentID: "agt_fixture", AgentSecret: "fixture-only", ControlPlaneURL: server.URL,
		HeartbeatSeconds: 1, BackoffMinSeconds: 1, BackoffMaxSeconds: 1}, exec, "unreleased", log)
	if err != nil {
		t.Fatal(err)
	}
	p.sendHeartbeat(context.Background())
	select {
	case body := <-bodies:
		return body
	default:
		t.Fatal("no heartbeat reached the control plane")
		return nil
	}
}

// The heartbeat carries the quarantine on its own, so a restore that
// left an application stopped is known without waiting for a refused result.
func TestHeartbeatCarriesRestoreQuarantine(t *testing.T) {
	want := []sdkclient.RestoreQuarantineReport{{DeploymentID: "dpl_aaaaaaaaaaaaaaaa", Scope: "application",
		Reason: "restore quiesce journal bkpjob_0123456789abcdef cannot be verified"}}
	body := heartbeatBody(t, quarantinedExec{reports: want})
	raw, ok := body["restore_quarantine"]
	if !ok {
		t.Fatalf("the heartbeat has no restore_quarantine: %v", body)
	}
	var got []sdkclient.RestoreQuarantineReport
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the quarantine changed on the wire: %s (%v)", raw, err)
	}
	// An agent with nothing quarantined sends no field at all.
	if clean := heartbeatBody(t, quarantinedExec{}); clean["restore_quarantine"] != nil {
		t.Fatalf("a clean agent reported a quarantine: %s", clean["restore_quarantine"])
	}
}
