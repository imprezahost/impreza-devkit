package impreza

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/libdns/libdns"
)

func TestF123DNSSecretDoesNotLeaveForAnotherHost(t *testing.T) {
	var hits int
	p := &Provider{
		AgentID:     "agt_fixture",
		AgentSecret: "synthetic-secret-value",
		BaseURL:     "https://f123-b.invalid",
		HTTPClient:  &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { hits++; return nil, http.ErrNoCookie })},
	}
	_, err := p.AppendRecords(context.Background(), "example.invalid.", []libdns.Record{libdns.RR{Type: "TXT", Name: "_acme", Data: "v"}})
	if err == nil || hits != 0 {
		t.Fatalf("dns secret left: err=%v hits=%d", err, hits)
	}
}

func TestF123DNSBlockCarriesTheBinding(t *testing.T) {
	p := &Provider{}
	d := caddyfile.NewTestDispenser(`dns impreza {
		agent_id agt_fixture
		agent_secret synthetic-secret-value
		base_url http://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.onion
		authorized_origin http://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.onion
	}`)
	if err := p.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if err := p.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
}

func TestF123DNSAcceptsTheAuthorizedOrigin(t *testing.T) {
	const onion = "http://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.onion"
	p := &Provider{AgentID: "agt_fixture", AgentSecret: "synthetic-secret-value", BaseURL: onion, AuthorizedOrigin: onion}
	if err := p.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	dev := &Provider{AgentID: "agt_fixture", AgentSecret: "synthetic-secret-value", BaseURL: "https://dev.example.invalid"}
	if err := dev.Provision(caddy.Context{}); err == nil {
		t.Fatal("DEV origin without a binding was accepted")
	}
	dev.AuthorizedOrigin = "https://dev.example.invalid"
	if err := dev.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
}

func TestF123DNSRefusesRedirect(t *testing.T) {
	var hitsB int
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.WriteHeader(204)
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/v1/agent/dns-challenge/present", http.StatusTemporaryRedirect)
	}))
	defer a.Close()
	p := &Provider{AgentID: "agt_fixture", AgentSecret: "synthetic-secret-value", BaseURL: a.URL}
	if err := p.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	_, err := p.AppendRecords(context.Background(), "example.invalid.", []libdns.Record{libdns.RR{Type: "TXT", Name: "_acme", Data: "v"}})
	if err == nil {
		t.Fatal("redirect was followed")
	}
	if hitsB != 0 {
		t.Fatalf("redirect target received the secret: %d", hitsB)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
