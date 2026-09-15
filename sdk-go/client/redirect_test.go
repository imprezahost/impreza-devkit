package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/imprezahost/impreza-devkit/sdk-go/config"
)

// The credentials this SDK sends are CUSTOM headers, and Go re-runs the auth
// transport on every redirect hop — so a followed 3xx would hand the customer's
// key and secret to whatever host it names. These tests pin the refusal for both
// realms, and prove the second host is never contacted at all.

func TestUserClientRefusesRedirectAndLeaksNothing(t *testing.T) {
	var attackerHits int64
	var gotKey, gotSecret string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attackerHits, 1)
		gotKey, gotSecret = r.Header.Get("X-API-Key"), r.Header.Get("X-API-Secret")
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/v1/account", http.StatusFound)
	}))
	defer api.Close()

	c, err := New(config.Context{Key: "imp_secret_key", Secret: "the_secret", URL: api.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.HTTP.Transport.(*retryTransport).maxAttempts = 1

	var out map[string]any
	err = c.Get(context.Background(), "/account", nil, &out)
	if err == nil {
		t.Fatal("expected the redirect to be refused, got success")
	}
	if !strings.Contains(err.Error(), "refusing to follow a redirect") {
		t.Fatalf("wrong error, redirect may have been followed: %v", err)
	}
	if n := atomic.LoadInt64(&attackerHits); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
	if gotKey != "" || gotSecret != "" {
		t.Fatalf("credentials reached the redirect target: key=%q secret=%q", gotKey, gotSecret)
	}
}

func TestAgentClientRefusesRedirect(t *testing.T) {
	var hits int64
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if r.Header.Get("X-Agent-Secret") != "" {
			t.Errorf("agent secret reached the redirect target")
		}
	}))
	defer attacker.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/v1/agent/poll", http.StatusTemporaryRedirect)
	}))
	defer api.Close()

	c, err := NewAgent(AgentOptions{AgentID: "agt_test", AgentSecret: "agts_secret", BaseURL: api.URL})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	c.HTTP.Transport.(*retryTransport).maxAttempts = 1

	var out map[string]any
	if err := c.Get(context.Background(), "/agent/poll", nil, &out); err == nil {
		t.Fatal("expected the redirect to be refused, got success")
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
}

// A same-host redirect is refused too: the point is that the API does not
// redirect at all, so following one would mean trusting a hop we never planned.
func TestRedirectRefusedEvenSameHost(t *testing.T) {
	var final int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/elsewhere" {
			atomic.AddInt64(&final, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
			return
		}
		http.Redirect(w, r, "/v1/elsewhere", http.StatusMovedPermanently)
	}))
	defer api.Close()

	c, err := New(config.Context{Key: "k", Secret: "s", URL: api.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.HTTP.Transport.(*retryTransport).maxAttempts = 1

	var out map[string]any
	if err := c.Get(context.Background(), "/account", nil, &out); err == nil {
		t.Fatal("expected same-host redirect to be refused")
	}
	if n := atomic.LoadInt64(&final); n != 0 {
		t.Fatalf("redirect was followed to the final path %d times", n)
	}
}
