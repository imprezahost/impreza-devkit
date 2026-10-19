package poll

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A marked vantage stays on the targets and results routes. Poll and the
// customer report stay at zero until the mark is removed, and then poll
// resumes.
func TestMarkedVantageDoesNotPollUntilTheMarkIsRemoved(t *testing.T) {
	var polls, reports, targets, results atomic.Int32
	var marked atomic.Bool
	marked.Store(true)
	ctx, stop := context.WithTimeout(context.Background(), 45*time.Second)
	defer stop()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/uptime/targets":
			targets.Add(1)
			if !marked.Load() {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"error":   map[string]string{"code": "forbidden", "message": "not a vantage"},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"protocol": 1, "interval_seconds": 15,
				"targets": []map[string]string{{
					"deployment_id": "dpl_vantage00000001",
					"scheme":        "https",
					"host":          "not-valid.onion",
					"kind":          "onion",
				}},
			}})
		case "/v1/agent/uptime/results":
			results.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/v1/agent/poll":
			polls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/v1/agent/report":
			reports.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	p := testPoller(t, server.URL, t.TempDir(), &receiptExecutor{})
	p.cfg.HeartbeatSeconds = 1
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && (targets.Load() == 0 || results.Load() == 0) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	if polls.Load() != 0 || reports.Load() != 0 || targets.Load() == 0 || results.Load() == 0 {
		t.Fatalf("marked vantage left the probe loop: polls=%d reports=%d targets=%d results=%d",
			polls.Load(), reports.Load(), targets.Load(), results.Load())
	}
	marked.Store(false)
	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && polls.Load() == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not stop")
	}
	if polls.Load() == 0 {
		t.Fatal("unmarked agent did not return to poll")
	}
}
