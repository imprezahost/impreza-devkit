package poll

import (
	"context"
	"encoding/json"
	"fmt"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// A persisted result must survive transport/authentication failures unchanged.
// In particular, never follow a redirect carrying agent credentials or allow a
// new poll before an actual acknowledgement. No executor/helper mock certifies
// Docker recovery; the leased VPS matrix covers that separate boundary.
func TestResultACKFaultMatrix(t *testing.T) {
	for _, status := range []string{"success", "failed", "cancelled", "timeout", "partial"} {
		for _, fault := range []string{"unavailable", "unauthorized", "redirect", "lost"} {
			t.Run(status+"/"+fault, func(t *testing.T) {
				var accepted atomic.Bool
				var posts, polls, sinks atomic.Int32
				var received []sdkclient.DeployResult
				done, stop := context.WithCancel(context.Background())
				defer stop()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Agent-Id") != "agt_fixture" || r.Header.Get("X-Agent-Secret") != "fixture" {
						t.Error("missing agent auth")
					}
					switch r.URL.Path {
					case "/v1/agent/report":
						w.WriteHeader(204)
					case "/v1/agent/command-progress":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"success":true,"data":{"command_id":"cmd_ack","terminal":false}}`)
					case "/v1/agent/poll":
						polls.Add(1)
						stop()
						w.WriteHeader(204)
					case "/sink":
						sinks.Add(1)
						w.WriteHeader(204)
					case "/v1/agent/deploy-result":
						var result sdkclient.DeployResult
						if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
							t.Error(err)
						}
						received = append(received, result)
						posts.Add(1)
						if accepted.Load() {
							w.WriteHeader(204)
							return
						}
						switch fault {
						case "unavailable":
							w.WriteHeader(503)
						case "unauthorized":
							w.WriteHeader(401)
						case "redirect":
							w.Header().Set("Location", "http://"+r.Host+"/sink")
							w.WriteHeader(307)
						case "lost":
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close()
						}
					default:
						t.Error("unexpected request", r.URL.Path)
						w.WriteHeader(500)
					}
				}))
				defer server.Close()
				dir := t.TempDir()
				e := &receiptExecutor{}
				p := testPoller(t, server.URL, dir, e)
				result := sdkclient.DeployResult{CommandID: "cmd_ack", ControlToken: "fixture-control", DeploymentID: "dpl_ack", Status: status, PreparationRestored: status == "cancelled", LogsTail: "preserved output"}
				record := &commandRecord{Version: 1, AgentID: "agt_fixture", ControlPlaneURL: server.URL, CommandID: result.CommandID, ControlToken: result.ControlToken, ProgressProtocol: sdkclient.DeploymentProgressProtocol, Result: &result}
				lock, err := p.journal.open()
				if err != nil {
					t.Fatal(err)
				}
				if err = p.journal.save(record); err != nil {
					lock.Close()
					t.Fatal(err)
				}
				lock.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				if err = p.Run(ctx); err != nil {
					cancel()
					t.Fatal(err)
				}
				cancel()
				saved, err := p.journal.load()
				if err != nil || !reflect.DeepEqual(saved, record) || posts.Load() != 1 || polls.Load() != 0 || sinks.Load() != 0 || e.calls.Load() != 0 {
					t.Fatalf("unacknowledged result changed/released: %v posts=%d polls=%d sinks=%d", err, posts.Load(), polls.Load(), sinks.Load())
				}
				accepted.Store(true)
				p2 := testPoller(t, server.URL, dir, e)
				ctx2, cancel2 := context.WithTimeout(done, 2*time.Second)
				defer cancel2()
				if err = p2.Run(ctx2); err != nil {
					t.Fatal(err)
				}
				saved, err = p2.journal.load()
				if err != nil || saved != nil || posts.Load() != 2 || polls.Load() != 1 || sinks.Load() != 0 || e.calls.Load() != 0 || len(received) != 2 || !reflect.DeepEqual(received[0], received[1]) {
					t.Fatal("ACK replay/cleanup mismatch", err)
				}
			})
		}
	}
}
