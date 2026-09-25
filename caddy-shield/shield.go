// Package caddy-shield bundles the Impreza Shield L7 protections that run
// inside the managed Caddy proxy container: a proof-of-work gate
// (`shield_pow`, a Go port of the Onion Guard algorithm) and an in-memory
// rate limiter with temporary bans (`shield_rate_limit`).
//
// Privacy contract: these handlers NEVER persist client identity.
// Client keys are HMAC-SHA256(pepper, source-ip) with a random per-process
// pepper; challenges are stored hashed; cookies are HMAC-signed tokens with
// no server-side session. Nothing touches disk. A Caddy restart or config
// reload invalidates in-memory state — visitors simply redo a cheap
// challenge. This is deliberate: no retained identity, no DDoS-volumetric
// claims (L7 only).
package caddyshield

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/prometheus/client_golang/prometheus"
)

// processPepper is the random per-process key. It exists only in memory and
// keys every HMAC (client keys, challenge store keys, cookies). A restart
// rotates it, invalidating all previously issued cookies and rate buckets —
// an accepted trade-off, never a persisted secret.
var processPepper = mustRandomHex(32)

func mustRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("caddy-shield: entropy source unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// hmacHex returns hex(HMAC-SHA256(pepper, msg)) — the canonical hashed key
// form used for every client-derived identifier.
func hmacHex(msg string) string {
	m := hmac.New(sha256.New, []byte(processPepper))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// constantTimeEqual compares two hex strings in constant time.
func constantTimeEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// clientKey hashes the request's source into a stable, non-reversible key.
// Loopback/unix sources (Tor forwards onion traffic through a local socket,
// so the visitor IP is not available there) collapse into one shared bucket —
// the same fallback the Onion Guard used when circuit IDs were absent.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return hmacHex("shared")
	}
	return hmacHex(host)
}

// cappedStore is a string→expiry map with a hard entry cap and lazy
// sweeping. It backs the challenge and ban stores so a flood of synthetic
// identifiers cannot grow memory without bound.
type cappedStore struct {
	mu    sync.Mutex
	max   int
	entry map[string]int64 // key → unix-nanos expiry
	now   func() int64
}

func newCappedStore(max int, now func() int64) *cappedStore {
	return &cappedStore{max: max, entry: make(map[string]int64), now: now}
}

// add stores key→expiry. Returns false when the store is at capacity after
// sweeping expired entries; callers decide their fail-open policy.
func (s *cappedStore) add(key string, expiryNanos int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if _, exists := s.entry[key]; !exists && len(s.entry) >= s.max {
		return false
	}
	s.entry[key] = expiryNanos
	return true
}

// has reports whether key exists and is unexpired, deleting it when expired.
func (s *cappedStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, exists := s.entry[key]
	if !exists {
		return false
	}
	if s.now() >= exp {
		delete(s.entry, key)
		return false
	}
	return true
}

// take is has() plus single-use consumption: it removes the key when present
// and unexpired.
func (s *cappedStore) take(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, exists := s.entry[key]
	if !exists {
		return false
	}
	delete(s.entry, key)
	return s.now() < exp
}

func (s *cappedStore) sweepLocked() {
	now := s.now()
	for k, exp := range s.entry {
		if now >= exp {
			delete(s.entry, k)
		}
	}
}

func (s *cappedStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entry)
}

// ─────────────────────────────────────────────────────────────────────
// Prometheus counters — aggregate only. Labels carry the deployment id and
// coarse outcomes; never an IP, path, user agent or rule payload.
// Registered once per process; reloads reuse the same vectors.
// ─────────────────────────────────────────────────────────────────────

var (
	powChallenges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_shield_pow_challenges_total",
		Help: "PoW challenges issued, by deployment.",
	}, []string{"deployment"})
	powPassed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_shield_pow_passed_total",
		Help: "PoW challenges solved, by deployment.",
	}, []string{"deployment"})
	powRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_shield_pow_rejected_total",
		Help: "PoW verify attempts refused (bad/expired/replayed), by deployment.",
	}, []string{"deployment"})
	rateRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_shield_ratelimit_rejected_total",
		Help: "Requests answered with 429 by the shield rate limiter, by deployment.",
	}, []string{"deployment"})
	rateBanned = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "impreza_shield_ratelimit_banned_total",
		Help: "Temporary bans applied by the shield rate limiter, by deployment.",
	}, []string{"deployment"})
)

// registerMetrics puts the package collectors on the CURRENT config's
// metrics registry — the one the admin /metrics endpoint actually gathers
// (found live on the 20260925 battery: the default registry is invisible
// there). Registries are per-config; several module instances share this
// process-wide collector set, so re-registration within one registry is
// tolerated.
func registerMetrics(ctx caddy.Context) {
	reg := ctx.GetMetricsRegistry()
	if reg == nil {
		return
	}
	for _, c := range []prometheus.Collector{
		powChallenges, powPassed, powRejected, rateRejected, rateBanned,
		proxyRequests, proxyLatency, proxyBytesIn, proxyBytesOut,
	} {
		if err := reg.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// Directive order, decided in ONE place. The chain must be
//
//	proxy_metrics -> shield_rate_limit -> shield_pow -> reverse_proxy
//
// (with coraza_waf ordered first via the global options block). The rate
// limiter sits BEFORE the PoW gate so unauthenticated challenge traffic is
// throttled too — found live on the 20260925 battery, where a gate-first
// order let challenge floods bypass the limiter entirely. proxy_metrics is
// outermost so gated and blocked responses still count.
func init() {
	httpcaddyfile.RegisterDirectiveOrder("proxy_metrics", httpcaddyfile.Before, "reverse_proxy")
	httpcaddyfile.RegisterDirectiveOrder("shield_rate_limit", httpcaddyfile.Before, "reverse_proxy")
	httpcaddyfile.RegisterDirectiveOrder("shield_pow", httpcaddyfile.Before, "reverse_proxy")
}

// parseDurationSeconds accepts Caddy-style duration values ("2m", "90s",
// "1h", plain seconds) and returns seconds. Empty strings yield 0.
func parseDurationSeconds(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	var n int64
	var unit string
	if _, err := fmt.Sscanf(raw, "%d%s", &n, &unit); err != nil {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative duration %q", raw)
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "s":
		return n, nil
	case "m":
		return n * 60, nil
	case "h":
		return n * 3600, nil
	case "d":
		return n * 86400, nil
	default:
		return 0, fmt.Errorf("unsupported duration unit in %q", raw)
	}
}

// caddyfileInt reads the next dispenser argument as an integer.
func caddyfileInt(d *caddyfile.Dispenser) (int, error) {
	if !d.NextArg() {
		return 0, d.ArgErr()
	}
	v, err := strconv.Atoi(d.Val())
	if err != nil {
		return 0, d.Errf("invalid integer %q", d.Val())
	}
	return v, nil
}
