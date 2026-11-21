package caddyshield

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreviewSharingIngress(t *testing.T) {
	credential := strings.Repeat("x", 43)
	digest := sha256.Sum256([]byte(credential))
	var active atomic.Bool
	active.Store(true)
	var calls atomic.Int32
	m := &PreviewSharingMiddleware{GrantID: "psg_" + strings.Repeat("1", 32), DeploymentID: "dpl_preview", Generation: 1,
		Destination: strings.Repeat("a", 56) + ".onion", ExpiresAt: time.Now().Unix() + 60, Journal: filepath.Join(t.TempDir(), "grant.json")}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var q map[string]any
		if json.NewDecoder(r.Body).Decode(&q) != nil || r.URL.Path != "/v1/agent/preview-sharing/authorize" || r.Header.Get("X-Agent-Id") != "fixture-agent" || r.Header.Get("X-Agent-Secret") != "fixture-secret" {
			t.Error("control plane binding missing")
			w.WriteHeader(403)
			return
		}
		encoded, _ := json.Marshal(q)
		if bytes.Contains(encoded, []byte(credential)) || len(q) != 6 || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
			t.Error("visitor metadata crossed policy boundary")
		}
		if !active.Load() || q["token_hash"] != hex.EncodeToString(digest[:]) {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": sharingPolicy{State: "active", Protocol: PreviewSharingIngressProtocol, Allowed: true, GrantID: m.GrantID, DeploymentID: m.DeploymentID, Generation: 1, Destination: m.Destination, ExpiresAt: m.ExpiresAt}})
	}))
	defer api.Close()
	m.APIURL = api.URL
	m.AgentID = "fixture-agent"
	m.AgentSecret = "fixture-secret"
	m.client = api.Client()
	m.client.Timeout = time.Second
	journal := func(state string) {
		t.Helper()
		q := map[string]any{"protocol": PreviewSharingIngressProtocol, "state": state, "grant_id": m.GrantID, "deployment_id": m.DeploymentID, "generation": m.Generation, "destination": m.Destination, "expires_at": m.ExpiresAt}
		b, _ := json.Marshal(q)
		if err := os.WriteFile(m.Journal, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	journal("active")
	var forwarded atomic.Int32
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		forwarded.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.Header.Get("X-Forwarded-For") != "" || r.RemoteAddr != "" {
			t.Error("visitor identity/credential forwarded to app")
		}
		w.Header().Set("Cache-Control", "public")
		w.Header().Set("Referrer-Policy", "unsafe-url")
		w.Header().Set("Set-Cookie", "session=unwanted")
		_, e := w.Write([]byte("private preview fixture"))
		return e
	})
	request := func(method, host, password, query string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://"+host+"/"+query, nil)
		r.Host = host
		if password != "" {
			r.SetBasicAuth("share", password)
		}
		r.Header.Set("Cookie", "PRIVATE_VISITOR_MARKER")
		r.Header.Set("Referer", "PRIVATE_VISITOR_MARKER")
		r.Header.Set("X-Forwarded-For", "PRIVATE_VISITOR_MARKER")
		w := httptest.NewRecorder()
		if err := m.ServeHTTP(w, r, next); err != nil {
			t.Fatal(err)
		}
		return w
	}
	t.Run("anonymous accountless visitor with separate credential", func(t *testing.T) {
		w := request("GET", m.Destination, credential, "")
		if w.Code != 200 || w.Body.String() != "private preview fixture" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Set-Cookie") != "" {
			t.Fatal("granted preview not protected")
		}
	})
	t.Run("informational 103 cannot bypass the final response policy", func(t *testing.T) {
		hinted := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Link", "<https://clearnet.invalid/track.css>; rel=preload; as=style")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Referrer-Policy", "unsafe-url")
			w.Header().Set("Set-Cookie", "fixture=tracking")
			w.Header().Set("Content-Security-Policy", "default-src *")
			_, e := w.Write([]byte("private preview fixture"))
			return e
		})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := m.ServeHTTP(w, r, hinted); err != nil {
				t.Error(err)
			}
		}))
		defer srv.Close()
		var informational []int
		r, _ := http.NewRequest("GET", srv.URL+"/", nil)
		r.Host = m.Destination
		r.SetBasicAuth("share", credential)
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			informational = append(informational, code)
			return nil
		}}))
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		h := resp.Header
		if resp.StatusCode != 200 || h.Get("Set-Cookie") != "" || h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" || !strings.HasPrefix(h.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Fatalf("final policy bypassed after 103: status %d, headers %v", resp.StatusCode, h)
		}
		if len(informational) != 0 {
			t.Fatalf("informational response with app-controlled preload hints reached the visitor: %v", informational)
		}
	})
	cases := []struct{ name, method, host, password, query string }{
		{"no credential", "GET", m.Destination, "", ""}, {"wrong credential", "GET", m.Destination, strings.Repeat("y", 43), ""},
		{"short credential", "GET", m.Destination, "short", ""}, {"sibling origin", "GET", strings.Repeat("b", 56) + ".onion", credential, ""},
		{"host port tampering", "GET", m.Destination + ":80", credential, ""}, {"host trailing dot", "GET", m.Destination + ".", credential, ""},
		{"URL credential", "GET", m.Destination, "", "?token=" + credential}, {"query rejected", "GET", m.Destination, credential, "?token=" + credential},
		{"POST mutation denied", "POST", m.Destination, credential, ""}, {"PUT mutation denied", "PUT", m.Destination, credential, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := request(c.method, c.host, c.password, c.query); w.Code != 401 {
				t.Fatalf("got %d", w.Code)
			}
		})
	}
	t.Run("each access rechecks server tombstone without positive cache", func(t *testing.T) {
		before := calls.Load()
		active.Store(false)
		defer active.Store(true)
		for i := 0; i < 3; i++ {
			if request("GET", m.Destination, credential, "").Code != 401 {
				t.Fatal("revoke reopened")
			}
		}
		if calls.Load() != before+3 {
			t.Fatal("cached policy")
		}
	})
	for _, state := range []string{"pending", "revoked", "unknown"} {
		t.Run("local journal "+state, func(t *testing.T) {
			journal(state)
			defer journal("active")
			before := calls.Load()
			if request("GET", m.Destination, credential, "").Code != 401 || calls.Load() != before {
				t.Fatal("invalid local journal admitted")
			}
		})
	}
	t.Run("missing local journal", func(t *testing.T) {
		os.Remove(m.Journal)
		defer journal("active")
		if request("GET", m.Destination, credential, "").Code != 401 {
			t.Fatal("missing journal admitted")
		}
	})
	t.Run("corrupt local journal", func(t *testing.T) {
		os.WriteFile(m.Journal, []byte("invalid"), 0600)
		defer journal("active")
		if request("GET", m.Destination, credential, "").Code != 401 {
			t.Fatal("corrupt journal admitted")
		}
	})
	t.Run("expired even when server healthy", func(t *testing.T) {
		old := m.ExpiresAt
		m.ExpiresAt = time.Now().Unix() - 1
		journal("active")
		defer func() { m.ExpiresAt = old; journal("active") }()
		before := calls.Load()
		if request("GET", m.Destination, credential, "").Code != 401 || calls.Load() != before {
			t.Fatal("expired grant admitted")
		}
	})
	t.Run("API storage outage closes access", func(t *testing.T) {
		old := m.APIURL
		m.APIURL = "https://127.0.0.1:1"
		defer func() { m.APIURL = old }()
		if request("GET", m.Destination, credential, "").Code != 401 {
			t.Fatal("outage admitted")
		}
	})
	t.Run("restart cannot reopen revoked local journal", func(t *testing.T) {
		journal("revoked")
		defer journal("active")
		fresh := *m
		m = &fresh
		if request("GET", m.Destination, credential, "").Code != 401 {
			t.Fatal("restart reopened")
		}
	})
	t.Run("concurrent access after revoke", func(t *testing.T) {
		active.Store(false)
		defer active.Store(true)
		errs := make(chan int, 32)
		for i := 0; i < 32; i++ {
			go func() {
				r := httptest.NewRequest("GET", "http://"+m.Destination+"/", nil)
				r.SetBasicAuth("share", credential)
				w := httptest.NewRecorder()
				m.ServeHTTP(w, r, next)
				errs <- w.Code
			}()
		}
		for i := 0; i < 32; i++ {
			if <-errs != 401 {
				t.Fatal("concurrent revoked access admitted")
			}
		}
	})

	t.Run("upstream error cannot return private diagnostics to outer Caddy", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://"+m.Destination+"/", nil)
		r.SetBasicAuth("share", credential)
		w := httptest.NewRecorder()
		e := m.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return errors.New("PRIVATE_UPSTREAM_MARKER") }))
		if e != nil || w.Code != 502 || strings.Contains(w.Body.String(), "PRIVATE_UPSTREAM_MARKER") {
			t.Fatal("upstream diagnostic escaped guard")
		}
	})
	if forwarded.Load() != 1 {
		t.Fatal("negative request reached preview app")
	}
}

func TestPreviewSharingCaddyfile(t *testing.T) {
	text := fmt.Sprintf("preview_sharing {\n grant_id psg_%s\n deployment_id dpl_preview\n generation 1\n destination %s.onion\n expires_at %d\n journal /config/preview-sharing/psg_%s.json\n api_url https://fixture.invalid\n agent_id fixture\n agent_secret fixture\n}\n", strings.Repeat("1", 32), strings.Repeat("a", 56), time.Now().Unix()+60, strings.Repeat("1", 32))
	m := new(PreviewSharingMiddleware)
	if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(text)); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://fixture.invalid", "https://user:secret@fixture.invalid", "https://fixture.invalid/path", "https://fixture.invalid?x=y", "https://fixture.invalid#frag"} {
		m.APIURL = bad
		if m.Validate() == nil {
			t.Fatal("untrusted control-plane destination accepted")
		}
	}
}
