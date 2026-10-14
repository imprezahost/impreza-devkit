package poll

// With the cleanup inventory at its limit (1024 receipts), acknowledging a
// backup result must not hold the next command.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestBackupInventoryFullDoesNotBlockPolling(t *testing.T) {
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
	dir, err := p.cleanupDirectory()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1024; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("exhausted-%04d.json", i)), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
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
	err = p.pollLoop(ctx)
	t.Logf("pollLoop err=%v polls=%d acks=%d execute=%d active_nil=%v", err, polls.Load(), acks.Load(), e.calls.Load(), p.active == nil)
	if err != nil || polls.Load() < 2 {
		t.Fatalf("full cleanup inventory holds polling: err=%v polls=%d", err, polls.Load())
	}
}
