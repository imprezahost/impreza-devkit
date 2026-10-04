package caddyshield

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func controlledRequest(t *testing.T, m *PowMiddleware, url, peer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", url, nil)
	r.RemoteAddr = peer
	rec := httptest.NewRecorder()
	if err := m.ServeHTTP(rec, r, wrapNext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("UPSTREAM")) }))); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestShieldControlsPathsAndTrustedPeer(t *testing.T) {
	m := newTestPow(t)
	m.Paths = []string{"/api"}
	m.TrustedSources = []string{"192.0.2.0/24"}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/api", "/apiary", "/public/../api/login", "/%61pi/login"} {
		r := controlledRequest(t, m, url, "198.18.0.1:1234")
		if r.Code != 429 {
			t.Fatalf("protected path passed: %s", url)
		}
	}
	if r := controlledRequest(t, m, "/public", "198.18.0.1:1234"); r.Body.String() != "UPSTREAM" {
		t.Fatal("public path challenged")
	}
	if r := controlledRequest(t, m, "/api/login", "192.0.2.7:1234"); r.Body.String() != "UPSTREAM" {
		t.Fatal("trusted peer challenged")
	}
	r := httptest.NewRequest("POST", "/api", nil)
	r.RemoteAddr = "198.18.0.1:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.7")
	r.Header.Set("Forwarded", "for=192.0.2.7")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, r, wrapNext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("forwarded peer trusted") })))
	if rec.Code != 429 {
		t.Fatal("forwarded header bypassed challenge")
	}
	rl := &RateLimitMiddleware{Deployment: "controls", Limit: 30, WindowSeconds: 60, TrustedSources: []string{"192.0.2.0/24"}}
	if err := rl.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 32; n++ {
		for _, peer := range []string{"192.0.2.7:1234", "198.18.0.1:1234"} {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = peer
			rec := httptest.NewRecorder()
			rl.ServeHTTP(rec, r, wrapNext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("UPSTREAM")) })))
			want := 200
			if strings.HasPrefix(peer, "198.18") && n >= 30 {
				want = 429
			}
			if rec.Code != want {
				t.Fatalf("rate trust decision %d = %d want %d", n, rec.Code, want)
			}
		}
	}
}

func TestShieldAttackAbsoluteDeadlineRestart(t *testing.T) {
	until := time.Now().Unix() + 1
	m := &PowMiddleware{Deployment: "attack", Difficulty: 2, MaxDifficulty: 4, Paths: []string{"/admin/"}, AttackUntil: until}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	// Config serialization is the restart boundary; the deadline stays absolute.
	saved, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var restarted PowMiddleware
	if err = json.Unmarshal(saved, &restarted); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	if r := controlledRequest(t, &restarted, "/public", "198.18.0.1:1234"); r.Code != 429 || !strings.Contains(r.Body.String(), `"difficulty":4`) {
		t.Fatal("attack not global at max difficulty")
	}
	time.Sleep(time.Until(time.Unix(until, 0)) + 20*time.Millisecond)
	if r := controlledRequest(t, &restarted, "/public", "198.18.0.1:1234"); r.Body.String() != "UPSTREAM" {
		t.Fatal("expired attack did not restore base paths")
	}
	if r := controlledRequest(t, &restarted, "/admin/login", "198.18.0.1:1234"); r.Code != 429 || !strings.Contains(r.Body.String(), `"difficulty":2`) {
		t.Fatal("base difficulty not restored")
	}
	// Re-delivering the same persisted config must never renew the deadline.
	if err = restarted.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	if r := controlledRequest(t, &restarted, "/public", "198.18.0.1:1234"); r.Body.String() != "UPSTREAM" {
		t.Fatal("replay renewed deadline")
	}
	restarted.BaselineDisabled = true
	if r := controlledRequest(t, &restarted, "/admin/login", "198.18.0.1:1234"); r.Body.String() != "UPSTREAM" {
		t.Fatal("off/standard not restored")
	}
}

func TestShieldAttackOldCookieRejected(t *testing.T) {
	m := newTestPow(t)
	rec := httptest.NewRecorder()
	m.issueCookie(rec)
	r := httptest.NewRequest("POST", "/", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	if !m.cookieValid(r) {
		t.Fatal("base cookie invalid")
	}
	m.AttackUntil = time.Now().Unix() + 3600
	if m.cookieValid(r) {
		t.Fatal("weak cookie bypassed attack")
	}
	rec = httptest.NewRecorder()
	m.issueCookie(rec)
	r = httptest.NewRequest("POST", "/", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	if !m.cookieValid(r) {
		t.Fatal("attack cookie invalid")
	}
	m.AttackUntil = 1
	if m.cookieValid(r) {
		t.Fatal("attack cookie replayed into base")
	}
}

func TestShieldAttackDoesNotChangeAdaptiveBase(t *testing.T) {
	m := newTestPow(t)
	m.AdaptiveThreshold = 1
	m.AttackUntil = time.Now().Unix() + 3600
	m.windowMin = time.Now().Unix() - 120
	m.windowHits = 100
	m.hotWindows = 1
	m.countAdaptive()
	if m.difficulty != m.Difficulty || m.windowHits != 100 || m.hotWindows != 1 {
		t.Fatal("temporary attack traffic changed prior adaptive base state")
	}
}
