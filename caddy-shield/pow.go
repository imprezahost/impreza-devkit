package caddyshield

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(&PowMiddleware{})
	httpcaddyfile.RegisterHandlerDirective("shield_pow", parsePowCaddyfile)
}

// VerifyPath is the fixed endpoint the challenge page posts solutions to.
// It lives under a dot-prefixed reserved prefix that applications are
// expected not to own; collisions are documented in the Shield guide.
const VerifyPath = "/.impreza-shield/verify"

// CookieName is the shield pass-through cookie. Bound by HMAC to the
// deployment, so a cookie minted for one site never satisfies another.
const CookieName = "impreza_shield"

// PowMiddleware gates a site behind a self-contained proof of work, the Go
// port of the Onion Guard gate: SHA-256 leading-hex-zeros with adaptive
// difficulty, HMAC-signed single-use challenges, HMAC-signed stateless
// cookies, in-memory state only.
type PowMiddleware struct {
	// Deployment labels metrics and binds cookies to this site.
	Deployment string `json:"deployment,omitempty"`
	// Difficulty is the starting count of required leading hex zeros.
	Difficulty int `json:"difficulty,omitempty"`
	// MaxDifficulty caps adaptive growth.
	MaxDifficulty int `json:"max_difficulty,omitempty"`
	// AdaptiveThreshold: sustained requests/minute above this raise
	// difficulty by one, up to MaxDifficulty; five quiet minutes step it
	// back down. 0 disables adaptation.
	AdaptiveThreshold int `json:"adaptive_threshold,omitempty"`
	// CookieTTLSeconds is the validity of the issued cookie.
	CookieTTLSeconds int `json:"cookie_ttl_seconds,omitempty"`
	// ChallengeTTLSeconds bounds challenge lifetime (short window).
	ChallengeTTLSeconds int `json:"challenge_ttl_seconds,omitempty"`
	// SecureCookie adds the Secure attribute. Set on clearnet (TLS) routes;
	// MUST be left off for plain-HTTP onion routes where Secure would make
	// the browser drop the cookie.
	SecureCookie bool `json:"secure_cookie,omitempty"`

	logger *zap.Logger `json:"-"`
	chal   *cappedStore
	// adaptive state (per deployment instance)
	mu         sync.Mutex
	windowMin  int64 // start of current 60s window (unix seconds)
	windowHits int64
	hotWindows int64 // consecutive windows over threshold
	difficulty int    // current effective difficulty
	lastHot    time.Time
}

// CaddyModule returns the Caddy module information.
func (*PowMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.shield_pow",
		New: func() caddy.Module { return &PowMiddleware{} },
	}
}

func (m *PowMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "deployment":
				if !d.AllArgs(&m.Deployment) {
					return d.ArgErr()
				}
			case "difficulty":
				v, err := caddyfileInt(d)
				if err != nil {
					return err
				}
				m.Difficulty = v
			case "max_difficulty":
				v, err := caddyfileInt(d)
				if err != nil {
					return err
				}
				m.MaxDifficulty = v
			case "adaptive_threshold":
				v, err := caddyfileInt(d)
				if err != nil {
					return err
				}
				m.AdaptiveThreshold = v
			case "cookie_ttl":
				if !d.NextArg() {
					return d.ArgErr()
				}
				secs, err := parseDurationSeconds(d.Val())
				if err != nil {
					return d.Err(err.Error())
				}
				m.CookieTTLSeconds = int(secs)
			case "challenge_ttl":
				if !d.NextArg() {
					return d.ArgErr()
				}
				secs, err := parseDurationSeconds(d.Val())
				if err != nil {
					return d.Err(err.Error())
				}
				m.ChallengeTTLSeconds = int(secs)
			case "secure_cookie":
				m.SecureCookie = true
			default:
				return d.Errf("unknown subdirective %q", d.Val())
			}
		}
	}
	return nil
}

func parsePowCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m PowMiddleware
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

// Provision validates configuration and seeds per-instance state. Invalid
// combinations fail loudly — a mis-gated site must not silently pass.
func (m *PowMiddleware) Provision(ctx caddy.Context) error {
	m.logger = caddy.Log()
	registerMetrics(ctx)
	if m.Deployment == "" {
		return fmt.Errorf("shield_pow: deployment is required")
	}
	if m.Difficulty < 1 || m.Difficulty > 8 {
		return fmt.Errorf("shield_pow: difficulty must be 1-8")
	}
	if m.MaxDifficulty == 0 {
		m.MaxDifficulty = m.Difficulty
	}
	if m.MaxDifficulty < m.Difficulty || m.MaxDifficulty > 8 {
		return fmt.Errorf("shield_pow: max_difficulty must be >= difficulty and <= 8")
	}
	if m.CookieTTLSeconds <= 0 {
		m.CookieTTLSeconds = 3600
	}
	if m.CookieTTLSeconds > 86400 {
		return fmt.Errorf("shield_pow: cookie_ttl must be <= 1 day")
	}
	if m.ChallengeTTLSeconds <= 0 {
		m.ChallengeTTLSeconds = 180
	}
	if m.ChallengeTTLSeconds > 600 {
		return fmt.Errorf("shield_pow: challenge_ttl must be <= 10 minutes")
	}
	if m.AdaptiveThreshold < 0 {
		return fmt.Errorf("shield_pow: adaptive_threshold cannot be negative")
	}
	m.chal = newCappedStore(65536, func() int64 { return time.Now().UnixNano() })
	m.difficulty = m.Difficulty
	return nil
}

func (m *PowMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.URL.Path == VerifyPath {
		return m.serveVerify(w, r)
	}
	if m.cookieValid(r) {
		m.countAdaptive()
		return next.ServeHTTP(w, r)
	}
	m.countAdaptive()
	return m.serveChallenge(w, r)
}

// countAdaptive maintains the rolling per-minute request count and steps
// difficulty up after sustained load and down after quiet periods.
func (m *PowMiddleware) countAdaptive() {
	if m.AdaptiveThreshold == 0 {
		return
	}
	now := time.Now().Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.windowMin == 0 {
		m.windowMin = now
	}
	if now-m.windowMin >= 60 {
		if m.windowHits > int64(m.AdaptiveThreshold) {
			m.hotWindows++
			m.lastHot = time.Now()
			if m.hotWindows >= 2 && m.difficulty < m.MaxDifficulty {
				m.difficulty++
				m.hotWindows = 0
			}
		} else {
			m.hotWindows = 0
			if m.difficulty > m.Difficulty && time.Since(m.lastHot) > 5*time.Minute {
				m.difficulty--
			}
		}
		m.windowMin = now
		m.windowHits = 0
	}
	m.windowHits++
}

// currentDifficulty snapshots the effective difficulty.
func (m *PowMiddleware) currentDifficulty() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.difficulty
}

// Cookie layout: "<exp-unix>.<nonce-hex>.<mac>" where
// mac = HMAC-SHA256(pepper, "<exp>|<nonce>|<deployment>"). The random nonce
// is carried in the cookie (it is public material — the MAC binds it) so the
// server can recompute without any stored session state.
func (m *PowMiddleware) cookieValid(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil || len(c.Value) > 128 {
		return false
	}
	exp, rest, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	nonce, mac, ok := strings.Cut(rest, ".")
	if !ok || !isHex(nonce) || !isHex(mac) {
		return false
	}
	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() >= expUnix {
		return false
	}
	want := hmacHex(exp + "|" + nonce + "|" + m.Deployment)
	return constantTimeEqual(want, strings.ToLower(mac))
}

// issueCookie writes the HMAC-signed pass-through cookie.
func (m *PowMiddleware) issueCookie(w http.ResponseWriter) {
	exp := time.Now().Add(time.Duration(m.CookieTTLSeconds) * time.Second).Unix()
	nonce := mustRandomHex(16)
	mac := hmacHex(fmt.Sprintf("%d|%s|%s", exp, nonce, m.Deployment))
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    fmt.Sprintf("%d.%s.%s", exp, nonce, mac),
		Path:     "/",
		MaxAge:   m.CookieTTLSeconds,
		HttpOnly: true,
		Secure:   m.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

// serveVerify validates a solved challenge and issues the cookie.
func (m *PowMiddleware) serveVerify(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil
	}
	if err := r.ParseForm(); err != nil {
		powRejected.WithLabelValues(m.Deployment).Inc()
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil
	}
	challenge := r.FormValue("challenge")
	nonce := r.FormValue("nonce")
	next := sanitizeNext(r.FormValue("next"))

	// Bounds before any hashing: bounded inputs only.
	if len(challenge) < 32 || len(challenge) > 64 || !isHex(challenge) {
		powRejected.WithLabelValues(m.Deployment).Inc()
		http.Error(w, "invalid challenge", http.StatusBadRequest)
		return nil
	}
	if len(nonce) < 1 || len(nonce) > 20 || !isDigits(nonce) {
		powRejected.WithLabelValues(m.Deployment).Inc()
		http.Error(w, "invalid nonce", http.StatusBadRequest)
		return nil
	}
	// Single-use, short-window: the store holds SHA256(challenge).
	chalKey := hex.EncodeToString(sha256sum(challenge))
	if !m.chal.take(chalKey) {
		powRejected.WithLabelValues(m.Deployment).Inc()
		http.Error(w, "challenge expired or already used", http.StatusForbidden)
		return nil
	}
	// Recompute the proof: SHA256(challenge + ":" + nonce) must lead with
	// the required count of hex zeros.
	sum := sha256.Sum256([]byte(challenge + ":" + nonce))
	if !strings.HasPrefix(hex.EncodeToString(sum[:]), strings.Repeat("0", m.currentDifficulty())) {
		powRejected.WithLabelValues(m.Deployment).Inc()
		http.Error(w, "proof does not meet difficulty", http.StatusForbidden)
		return nil
	}
	if next == "" {
		next = "/"
	}
	m.issueCookie(w)
	powPassed.WithLabelValues(m.Deployment).Inc()
	http.Redirect(w, r, next, http.StatusSeeOther)
	return nil
}

// serveChallenge answers with the self-contained challenge page (browsers)
// or a machine-solvable JSON description (programmatic clients). HEAD gets a
// bare 200 without consuming a challenge — uptime probes must not drain the
// short-window store.
func (m *PowMiddleware) serveChallenge(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodHead {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return nil
	}
	difficulty := m.currentDifficulty()
	challenge := mustRandomHex(16)
	ttl := time.Duration(m.ChallengeTTLSeconds) * time.Second
	if !m.chal.add(hex.EncodeToString(sha256sum(challenge)), time.Now().Add(ttl).UnixNano()) {
		// Store saturated after sweeping: fail open with a challenge rather
		// than fail closed — an attacker flooding challenge issues must not
		// take the site down for legitimate visitors.
		m.logger.Warn("shield_pow: challenge store saturated; serving challenge without registration")
	}
	powChallenges.WithLabelValues(m.Deployment).Inc()
	next := sanitizeNext(r.URL.RequestURI())

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, renderChallengePage(challenge, difficulty, next))
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	return json.NewEncoder(w).Encode(map[string]any{
		"error":      "proof of work required",
		"challenge":  challenge,
		"difficulty": difficulty,
		"algorithm":  "sha256",
		"verify":     VerifyPath,
		"hint":       "find nonce so sha256hex(challenge + \":\" + nonce) starts with " + strings.Repeat("0", difficulty) + "; POST challenge, nonce, next to " + VerifyPath,
	})
}

// sanitizeNext keeps redirects same-origin: path+query only.
func sanitizeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	if len(raw) > 2048 {
		return ""
	}
	return raw
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func sha256sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Interface guard.
var _ caddyhttp.MiddlewareHandler = (*PowMiddleware)(nil)
