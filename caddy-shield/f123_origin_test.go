package caddyshield

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestF123OfficialPreviewParsesWithAndWithoutTheVariable(t *testing.T) {
	src, err := os.ReadFile("../agent-go/internal/proxy/preview_sharing.go")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "authorized_origin "
	at := strings.Index(string(src), marker)
	if at < 0 {
		t.Fatal("the agent fragment has no authorized origin")
	}
	rest := string(src)[at:]
	end := strings.IndexAny(rest, "\\\n")
	if end < 0 {
		t.Fatal("the agent fragment line is unfinished")
	}
	line := strings.TrimSpace(rest[:end])
	const dev = "https://dev.example.invalid"
	grant := "psg_" + strings.Repeat("1", 32)
	snippet := func(api string) string {
		return fmt.Sprintf("http://fixture.test {\n  preview_sharing {\n    grant_id %s\n    deployment_id dpl_preview\n    generation 1\n    destination %s\n    expires_at 9999999999\n    journal /config/preview-sharing/%s.json\n    api_url %s\n    %s\n    agent_id agt_fixture\n    agent_secret synthetic-secret-value\n  }\n}\n", grant, strings.Repeat("a", 56)+".onion", grant, api, line)
	}
	t.Run("official", func(t *testing.T) {
		t.Setenv("IMPREZA_ORIGIN_AUTHORIZED", "")
		m := parsePreviewFragment(t, snippet("https://api.imprezahost.com"))
		if m.AuthorizedOrigin != "" {
			t.Fatalf("official agent kept a binding: %q", m.AuthorizedOrigin)
		}
	})
	t.Run("bound", func(t *testing.T) {
		t.Setenv("IMPREZA_ORIGIN_AUTHORIZED", dev)
		m := parsePreviewFragment(t, snippet(dev))
		if m.AuthorizedOrigin != dev {
			t.Fatalf("bound origin was lost: %q", m.AuthorizedOrigin)
		}
	})
}

func parsePreviewFragment(t *testing.T, snippet string) *PreviewSharingMiddleware {
	t.Helper()
	blocks, err := caddyfile.Parse("Testfile", []byte(snippet))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("server blocks: %d", len(blocks))
	}
	m := &PreviewSharingMiddleware{}
	if err := m.UnmarshalCaddyfile(blocks[0].DispenseDirective("preview_sharing")); err != nil {
		t.Fatal(err)
	}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestF123PreviewSendsToTheAuthorizedOrigin(t *testing.T) {
	var hits int
	m := &PreviewSharingMiddleware{
		APIURL:           "https://dev.example.invalid",
		AuthorizedOrigin: "https://dev.example.invalid",
		AgentID:          "agt_fixture",
		AgentSecret:      "synthetic-secret-value",
		client: &http.Client{Transport: previewRoundTrip(func(*http.Request) (*http.Response, error) {
			hits++
			return nil, http.ErrNoCookie
		})},
	}
	if m.authorize(context.Background(), "credential") {
		t.Fatal("the fixture transport should not look like success")
	}
	if hits != 1 {
		t.Fatalf("authorized DEV origin was not contacted: %d", hits)
	}
	m.AuthorizedOrigin = ""
	hits = 0
	if m.authorize(context.Background(), "credential") || hits != 0 {
		t.Fatal("unbound DEV origin received the secret")
	}
}

func TestF123PreviewSecretDoesNotLeave(t *testing.T) {
	var hits int
	m := &PreviewSharingMiddleware{
		APIURL:      "https://f123-b.invalid",
		AgentID:     "agt_fixture",
		AgentSecret: "synthetic-secret-value",
		client: &http.Client{Transport: previewRoundTrip(func(*http.Request) (*http.Response, error) {
			hits++
			return nil, http.ErrNoCookie
		})},
	}
	if m.authorize(context.Background(), "credential") {
		t.Fatal("unbound preview host was authorized")
	}
	if hits != 0 {
		t.Fatal("the agent secret left toward f123-b.invalid")
	}
}

type previewRoundTrip func(*http.Request) (*http.Response, error)

func (f previewRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
