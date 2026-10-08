package client

// An ENDLESS body, paced (~64 MB/s) so that a read without the ceiling can never finish inside
// the deadline and stays bounded in this test (about 200 MB in 3 s). With the ceiling the read stops after 4 MiB + 1
// and answers the ceiling error in well under a second; without the LimitReader it keeps reading until the deadline
// (in production: until the memory is gone). The finite-body test cannot tell these apart: it serves a finite
// 4 MiB + 1 body, and an unbounded read followed by the length check still answers "ceiling".

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func endlessServer(t *testing.T) *httptest.Server {
	chunk := []byte(strings.Repeat("x", 64<<10))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}))
}

func ceiling(t *testing.T, what string, call func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := call(ctx)
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("%s: an endless body must stop at the ceiling, got after %s: %.200v", what, time.Since(start), err)
	}
}

func TestResponseReadStopsAnEndlessBodyAtTheCeiling(t *testing.T) {
	srv := endlessServer(t)
	defer srv.Close()
	c, err := NewAgent(AgentOptions{AgentID: "agt_limitend", AgentSecret: "fixture", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	ceiling(t, "Get (doWithHeaders)", func(ctx context.Context) error { return c.Get(ctx, "/v1/agent/uptime/targets", nil, &out) })
	ceiling(t, "Post (doWithHeaders)", func(ctx context.Context) error {
		return c.Post(ctx, "/v1/agent/uptime/results", map[string]any{}, &out)
	})
	ceiling(t, "GetRaw", func(ctx context.Context) error { _, err := c.GetRaw(ctx, "/v1/agent/uptime/targets", nil); return err })
	ceiling(t, "PostRaw", func(ctx context.Context) error {
		return c.PostRaw(ctx, "/v1/agent/uptime/results", "application/json", []byte("{}"), &out)
	})
	ceiling(t, "Bootstrap", func(ctx context.Context) error {
		_, err := Bootstrap(ctx, "fixture-token", BootstrapRequest{}, BootstrapOptions{BaseURL: srv.URL})
		return err
	})
}
