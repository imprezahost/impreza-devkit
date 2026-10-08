package client

// A compromised or buggy control plane must not exhaust a probe
// point's memory through an unbounded response body. Every read goes
// through the declared ceiling and answers a loud error above it.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseReadRefusesABodyAboveTheCeiling(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), MaxResponseBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(huge)
	}))
	defer srv.Close()
	c, err := NewAgent(AgentOptions{AgentID: "agt_limit", AgentSecret: "fixture", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	err = c.Get(context.Background(), "/v1/agent/uptime/targets", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("an over-ceiling body must answer the ceiling error, got: %v", err)
	}
	// The same ceiling on the POST path (results reporting).
	err = c.Post(context.Background(), "/v1/agent/uptime/results", map[string]any{}, &out)
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("the POST read must carry the same ceiling, got: %v", err)
	}
}

func TestResponseReadAcceptsABodyInsideTheCeiling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"targets":[],"interval_seconds":60}}`))
	}))
	defer srv.Close()
	c, err := NewAgent(AgentOptions{AgentID: "agt_limit", AgentSecret: "fixture", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := c.Get(context.Background(), "/v1/agent/uptime/targets", nil, &out); err != nil {
		t.Fatalf("an in-ceiling body must not trip the ceiling: %v", err)
	}
}
