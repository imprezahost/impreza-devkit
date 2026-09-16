package client

import (
	"context"
	"errors"
	"fmt"
	"github.com/imprezahost/impreza-devkit/sdk-go/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryCancellationHTTPMatrix(t *testing.T) {
	for _, realm := range []string{"agent", "api-key"} {
		for _, signal := range []string{"cancel", "deadline", "client-timeout"} {
			for _, fault := range []string{"429", "502", "503", "504", "date", "connection"} {
				t.Run(realm+"/"+signal+"/"+fault, func(t *testing.T) {
					var calls atomic.Int32
					arrived := make(chan struct{}, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if realm == "agent" {
							if r.Header.Get("X-Agent-Id") != "agt_retry" || r.Header.Get("X-Agent-Secret") != "fixture" {
								t.Error("missing agent auth")
							}
						} else if r.Header.Get("X-API-Key") != "imp_retry" || r.Header.Get("X-API-Secret") != "fixture" {
							t.Error("missing API auth")
						}
						select {
						case arrived <- struct{}{}:
						default:
						}
						if fault == "connection" {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close()
							return
						}
						status, _ := strconv.Atoi(fault)
						if fault == "date" {
							status = 429
							w.Header().Set("Retry-After", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
						} else {
							w.Header().Set("Retry-After", "60")
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						fmt.Fprint(w, `{"success":false,"error":{"code":"UNAVAILABLE","message":"fixture"}}`)
					}))
					defer server.Close()
					var c *Client
					var err error
					if realm == "agent" {
						c, err = NewAgent(AgentOptions{AgentID: "agt_retry", AgentSecret: "fixture", BaseURL: server.URL})
					} else {
						c, err = New(config.Context{Key: "imp_retry", Secret: "fixture", URL: server.URL})
					}
					if err != nil {
						t.Fatal(err)
					}
					c.HTTP.Transport.(*retryTransport).baseDelay = 30 * time.Second
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					want := context.Canceled
					if signal == "deadline" {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeout(ctx, 120*time.Millisecond)
						defer stop()
						want = context.DeadlineExceeded
					}
					if signal == "client-timeout" {
						c.HTTP.Timeout = 120 * time.Millisecond
						want = context.DeadlineExceeded
					}
					done := make(chan error, 1)
					started := time.Now()
					go func() {
						if realm == "agent" {
							_, err := c.AgentCommandControl(ctx, DeploymentControl{CommandID: "cmd_retry", ControlToken: "fixture-control", Phase: "preparing"})
							done <- err
						} else {
							_, err := c.AccountInfo(ctx)
							done <- err
						}
					}()
					select {
					case <-arrived:
					case <-time.After(2 * time.Second):
						t.Fatal("request did not arrive")
					}
					if signal == "cancel" {
						time.Sleep(20 * time.Millisecond)
						cancel()
					}
					select {
					case err := <-done:
						if !errors.Is(err, want) {
							t.Fatalf("expected %v, got %v", want, err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("cancellation blocked in retry wait")
					}
					if time.Since(started) > 2*time.Second || calls.Load() != 1 {
						t.Fatalf("deadline overrun or authenticated retry after cancellation: %s / %d", time.Since(started), calls.Load())
					}
				})
			}
		}
	}
}

func TestRetryAlreadyCancelledSendsNothing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	c, err := NewAgent(AgentOptions{AgentID: "agt_retry", AgentSecret: "fixture", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.AgentCommandControl(ctx, DeploymentControl{CommandID: "cmd_retry", ControlToken: "fixture", Phase: "preparing"})
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatal("cancelled request was sent", err, calls.Load())
	}
}
func TestRetryAfterSecondsCannotOverflow(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"9223372036854775807", 60 * time.Second}, {"60", 60 * time.Second}, {"61", 60 * time.Second}, {"1", time.Second}, {"0", 0}, {"-1", 0}, {"-9223372036854775808", 0}, {"invalid", 0}} {
		t.Run(tc.value, func(t *testing.T) {
			r := &http.Response{Header: http.Header{"Retry-After": []string{tc.value}}}
			if got := parseRetryAfter(r); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestRetrySuccessAndPermanentErrors(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504, 400, 401, 403, 404, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			var first string
			var bodyMu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				n := calls.Add(1)
				bodyMu.Lock()
				if n == 1 {
					first = string(raw)
				} else if string(raw) != first {
					t.Error("authenticated command body changed on retry")
				}
				bodyMu.Unlock()
				if r.Header.Get("X-Agent-Id") != "agt_retry" || r.Header.Get("X-Agent-Secret") != "fixture" {
					t.Error("auth changed")
				}
				if n == 1 {
					errorResponder(status, "FIXTURE", "fixture")(w, r)
					return
				}
				envelopeResponder(DeploymentControlResponse{CommandID: "cmd_retry", Phase: "preparing"})(w, r)
			}))
			defer server.Close()
			c, err := NewAgent(AgentOptions{AgentID: "agt_retry", AgentSecret: "fixture", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			c.HTTP.Transport.(*retryTransport).baseDelay = time.Millisecond
			result, err := c.AgentCommandControl(context.Background(), DeploymentControl{CommandID: "cmd_retry", ControlToken: "fixture-control", Phase: "preparing"})
			if isRetriableStatus(status) {
				if err != nil || result.CommandID != "cmd_retry" || calls.Load() != 2 {
					t.Fatal("transient failure did not retry correctly", err, calls.Load())
				}
			} else if err == nil || calls.Load() != 1 {
				t.Fatal("permanent error retried", err, calls.Load())
			}
		})
	}
}
