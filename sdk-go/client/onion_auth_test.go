package client

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestPlatformOnionAuthListHitsCollectionRoute(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/platform/deployments/dpl_test/onion/clients" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"restricted": true,
				"clients":    []map[string]string{{"name": "alice", "created_at": "2026-09-21T00:00:00Z"}},
			},
		})
	}))
	out, err := c.PlatformOnionAuthList(context.Background(), "dpl_test")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Restricted || len(out.Clients) != 1 || out.Clients[0].Name != "alice" {
		t.Fatalf("unexpected payload: %+v", out)
	}
	if out.Clients[0].CreatedAt.IsZero() {
		t.Fatal("created_at did not decode")
	}
}

func TestPlatformOnionAuthAddSendsPubkeyMode(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/platform/deployments/dpl_test/onion/clients" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["name"] != "alice" || body["pubkey"] != "x25519-pub" || len(body) != 2 {
			t.Errorf("unexpected body: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]string{"name": "alice", "pubkey": "x25519-pub"},
		})
	}))
	out, err := c.PlatformOnionAuthAdd(context.Background(), "dpl_test", OnionAuthAddRequest{Name: "alice", Pubkey: "x25519-pub"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pubkey != "x25519-pub" || out.PrivateKey != "" {
		t.Fatalf("pubkey mode must not return a private key: %+v", out)
	}
}

func TestPlatformOnionAuthAddGenerateReturnsOneTimePrivateKey(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["name"] != "bob" || body["generate"] != true || len(body) != 2 {
			t.Errorf("unexpected body: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]string{"name": "bob", "pubkey": "gen-pub", "private_key": "gen-priv"},
		})
	}))
	out, err := c.PlatformOnionAuthAdd(context.Background(), "dpl_test", OnionAuthAddRequest{Name: "bob", Generate: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.PrivateKey != "gen-priv" {
		t.Fatalf("generate mode must surface the one-time private key: %+v", out)
	}
}

func TestPlatformOnionAuthAddRejectsAmbiguousOrEmptyInput(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP call expected for invalid input")
	}))
	for _, req := range []OnionAuthAddRequest{
		{Name: "alice"}, // neither pubkey nor generate
		{Name: "alice", Pubkey: "p", Generate: true}, // both
		{Pubkey: "p"}, // missing name
	} {
		if _, err := c.PlatformOnionAuthAdd(context.Background(), "dpl_test", req); err == nil {
			t.Errorf("expected validation error for %+v", req)
		}
	}
}

func TestPlatformOnionAuthRevokeDeletesNamedClient(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/v1/platform/deployments/dpl_test/onion/clients/alice" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	if err := c.PlatformOnionAuthRevoke(context.Background(), "dpl_test", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := c.PlatformOnionAuthRevoke(context.Background(), "dpl_test", ""); err == nil {
		t.Fatal("empty client name must be refused before any HTTP call")
	}
}
