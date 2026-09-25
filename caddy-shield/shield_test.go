package caddyshield

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
)

// ─────────────────────────────────────────────────────────────────────
// PoW middleware — hermetic handler tests (no Caddy runtime needed).
// ─────────────────────────────────────────────────────────────────────

func newTestPow(t *testing.T) *PowMiddleware {
	t.Helper()
	m := &PowMiddleware{
		Deployment:     "dpl_test",
		Difficulty:     2,
		MaxDifficulty:  4,
		CookieTTLSeconds: 60,
		ChallengeTTLSeconds: 60,
	}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	return m
}

func solve(challenge string, difficulty int) (string, string) {
	prefix := strings.Repeat("0", difficulty)
	for nonce := 0; ; nonce++ {
		sum := sha256.Sum256([]byte(challenge + ":" + strconv.Itoa(nonce)))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), prefix) {
			return challenge, strconv.Itoa(nonce)
		}
	}
}

func extractChallenge(t *testing.T, body string) string {
	t.Helper()
	// The JSON variant is easier to parse; drive the non-GET path.
	if i := strings.Index(body, `"challenge":"`); i >= 0 {
		rest := body[i+len(`"challenge":"`):]
		return rest[:strings.Index(rest, `"`)]
	}
	t.Fatalf("no challenge in body: %.200s", body)
	return ""
}

func TestPowGateThenPass(t *testing.T) {
	m := newTestPow(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "UPSTREAM")
	})

	// 1. No cookie → GET yields the challenge page (200, HTML, no-store).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/some/path?x=1", nil)
	if err := m.ServeHTTP(rec, req, wrapNext(next)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge page status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("challenge content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	if strings.Contains(rec.Body.String(), "UPSTREAM") {
		t.Fatal("upstream leaked without a solved challenge")
	}

	// 2. Non-GET without cookie → machine-solvable JSON challenge (429).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api", nil)
	if err := m.ServeHTTP(rec, req, wrapNext(next)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("json challenge status = %d", rec.Code)
	}
	challenge := extractChallenge(t, rec.Body.String())

	// 3. Solve and verify → 303 + cookie.
	chal, nonce := solve(challenge, m.Difficulty)
	form := formReader(map[string]string{"challenge": chal, "nonce": nonce, "next": "/some/path?x=1"})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, VerifyPath, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := m.ServeHTTP(rec, req, wrapNext(next)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("verify status = %d body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/some/path?x=1" {
		t.Fatalf("verify redirect = %q", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName {
		t.Fatalf("cookie missing: %+v", cookies)
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags wrong: %+v", c)
	}
	if c.Secure { // onion-style route here: SecureCookie unset
		t.Fatalf("Secure set while secure_cookie disabled")
	}

	// 4. Cookie pass → upstream.
	req = httptest.NewRequest(http.MethodGet, "/some/path", nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	if err := m.ServeHTTP(rec, req, wrapNext(next)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "UPSTREAM" {
		t.Fatalf("upstream not reached after valid cookie: %s", rec.Body.String())
	}
}

func TestPowChallengeSingleUse(t *testing.T) {
	m := newTestPow(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	_ = m.ServeHTTP(rec, req, wrapNext(next))
	challenge := extractChallenge(t, rec.Body.String())
	chal, nonce := solve(challenge, m.Difficulty)

	post := func() *httptest.ResponseRecorder {
		form := formReader(map[string]string{"challenge": chal, "nonce": nonce})
		r := httptest.NewRequest(http.MethodPost, VerifyPath, form)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		_ = m.ServeHTTP(w, r, wrapNext(next))
		return w
	}
	if got := post().Code; got != http.StatusSeeOther {
		t.Fatalf("first verify = %d", got)
	}
	if got := post().Code; got != http.StatusForbidden {
		t.Fatalf("replay must be rejected, got %d", got)
	}
}

func TestPowWrongDifficultyRejected(t *testing.T) {
	m := newTestPow(t) // difficulty 2
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	_ = m.ServeHTTP(rec, req, wrapNext(next))
	challenge := extractChallenge(t, rec.Body.String())

	// A nonce whose digest has no leading zero at all cannot meet
	// difficulty 2 — deterministic, unlike "exactly one zero".
	var chal, nonce string
	chal = challenge
	for n := 0; ; n++ {
		sum := sha256.Sum256([]byte(challenge + ":" + strconv.Itoa(n)))
		h := hex.EncodeToString(sum[:])
		if !strings.HasPrefix(h, "0") {
			nonce = strconv.Itoa(n)
			break
		}
	}
	form := formReader(map[string]string{"challenge": chal, "nonce": nonce})
	req = httptest.NewRequest(http.MethodPost, VerifyPath, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	_ = m.ServeHTTP(rec, req, wrapNext(next))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("weak proof accepted: %d", rec.Code)
	}
}

func TestPowCookieBoundToDeployment(t *testing.T) {
	m1 := newTestPow(t)
	m2 := newTestPow(t)
	m2.Deployment = "dpl_other"
	if err := m2.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	_ = m1.ServeHTTP(rec, req, wrapNext(next))
	challenge := extractChallenge(t, rec.Body.String())
	chal, nonce := solve(challenge, m1.Difficulty)
	form := formReader(map[string]string{"challenge": chal, "nonce": nonce})
	req = httptest.NewRequest(http.MethodPost, VerifyPath, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	_ = m1.ServeHTTP(rec, req, wrapNext(next))
	cookie := rec.Result().Cookies()[0]

	// Same cookie against another deployment's gate must NOT pass.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	_ = m2.ServeHTTP(rec, req, wrapNext(next))
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "UPSTREAM") {
		t.Fatal("cross-deployment cookie accepted")
	}
}

func TestPowForgedCookieRejected(t *testing.T) {
	m := newTestPow(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "UPSTREAM") })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Well-formed layout, bogus MAC.
	exp := "99999999999"
	req.AddCookie(&http.Cookie{Name: CookieName, Value: exp + ".00112233445566778899aabbccddeeff." + strings.Repeat("ab", 32)})
	rec := httptest.NewRecorder()
	_ = m.ServeHTTP(rec, req, wrapNext(next))
	if strings.Contains(rec.Body.String(), "UPSTREAM") {
		t.Fatal("forged cookie passed the gate")
	}
}

func TestPowNextSanitization(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"/ok":                     "/ok",
		"//evil.example/steal":    "",
		"https://evil.example":    "",
		"javascript:alert(1)":     "",
	}
	for in, want := range cases {
		if got := sanitizeNext(in); got != want {
			t.Fatalf("sanitizeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPowSecureCookieFlag(t *testing.T) {
	m := newTestPow(t)
	m.SecureCookie = true
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	_ = m.ServeHTTP(rec, req, wrapNext(next))
	challenge := extractChallenge(t, rec.Body.String())
	chal, nonce := solve(challenge, m.Difficulty)
	form := formReader(map[string]string{"challenge": chal, "nonce": nonce})
	req = httptest.NewRequest(http.MethodPost, VerifyPath, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	_ = m.ServeHTTP(rec, req, wrapNext(next))
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName && !c.Secure {
			t.Fatal("secure_cookie requested but Secure attribute missing")
		}
	}
}

func TestPowPageContainsNoVisitorData(t *testing.T) {
	page := renderChallengePage("aabb", 3, "/p?x=<script>")
	lower := strings.ToLower(page)
	for _, leak := range []string{"remoteaddr", "192.0.", "127.0.0.1", "user-agent"} {
		if strings.Contains(lower, leak) {
			t.Fatalf("page leaks %q", leak)
		}
	}
	if !strings.Contains(page, `<script>`) {
		t.Fatal("inline solver missing")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Rate limit middleware.
// ─────────────────────────────────────────────────────────────────────

func TestRateLimitWindowAndBan(t *testing.T) {
	m := &RateLimitMiddleware{Deployment: "dpl_test", Limit: 10, WindowSeconds: 60, BanSeconds: 60, StrikeFactor: 3}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "OK") })

	over := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = key + ":1234"
		rec := httptest.NewRecorder()
		_ = m.ServeHTTP(rec, req, wrapNext(next))
		return rec
	}

	// 10 allowed.
	for i := 0; i < 10; i++ {
		if rec := over("10.1.1.7"); rec.Code != http.StatusOK {
			t.Fatalf("request %d rejected: %d", i, rec.Code)
		}
	}
	// 11th..: 429 with Retry-After.
	rec := over("10.1.1.7")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	// Another key unaffected (per-key isolation).
	if rec := over("10.1.1.8"); rec.Code != http.StatusOK {
		t.Fatalf("second key punished: %d", rec.Code)
	}
	// Reaching strike factor lands the ban: already at 11; push to 30+.
	for i := 0; i < 25; i++ {
		rec = over("10.1.1.7")
		if rec.Code == http.StatusForbidden {
			break
		}
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected ban 403, got %d", rec.Code)
	}
	// Banned key stays 403 even under the limit now.
	if rec := over("10.1.1.7"); rec.Code != http.StatusForbidden {
		t.Fatalf("ban not enforced: %d", rec.Code)
	}
}

func TestRateLimitSharedBucketForLoopback(t *testing.T) {
	m := &RateLimitMiddleware{Deployment: "dpl_test", Limit: 10, WindowSeconds: 60, BanSeconds: 60, StrikeFactor: 3}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "OK") })
	get := func(remote string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		_ = m.ServeHTTP(rec, req, wrapNext(next))
		return rec.Code
	}
	// Onion-style traffic arrives from the local socket: shared bucket.
	for i := 0; i < 10; i++ {
		if code := get("@"); code != http.StatusOK {
			t.Fatalf("unix request %d rejected: %d", i, code)
		}
	}
	if code := get("127.0.0.1:9999"); code != http.StatusTooManyRequests {
		t.Fatalf("loopback did not share the bucket: %d", code)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Shared helpers.
// ─────────────────────────────────────────────────────────────────────

// formReader encodes the pairs as a real application/x-www-form-urlencoded
// body using net/url, so ParseForm sees exactly one well-formed stream.
func formReader(pairs map[string]string) io.Reader {
	vals := url.Values{}
	for k, v := range pairs {
		vals.Set(k, v)
	}
	return strings.NewReader(vals.Encode())
}

func wrapNext(h http.Handler) handlerFunc { return handlerFunc(h.ServeHTTP) }

type handlerFunc func(http.ResponseWriter, *http.Request)

func (f handlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) error { f(w, r); return nil }
