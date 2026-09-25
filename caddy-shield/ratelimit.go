package caddyshield

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(&RateLimitMiddleware{})
	httpcaddyfile.RegisterHandlerDirective("shield_rate_limit", parseRateLimitCaddyfile)
}

// RateLimitMiddleware is the Shield in-memory L7 rate limiter with temporary
// bans. Keys are HMAC-hashed client identifiers (never raw IPs); state is
// per-process only. Exceeding Limit inside Window answers 429; sustained
// abuse (StrikeFactor× the limit in one window) lands a temporary ban (403
// for BanDuration). This is abuse protection, not volumetric DDoS
// mitigation.
type RateLimitMiddleware struct {
	Deployment   string `json:"deployment,omitempty"`
	WindowSeconds int   `json:"window_seconds,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	BanSeconds   int    `json:"ban_seconds,omitempty"`
	// StrikeFactor: requests beyond StrikeFactor×Limit in one window
	// trigger the temporary ban.
	StrikeFactor int `json:"strike_factor,omitempty"`

	logger *zap.Logger `json:"-"`

	mu      sync.Mutex
	buckets map[string]*rlBucket
	bans    *cappedStore
}

type rlBucket struct {
	windowStart int64
	count       int64
}

// CaddyModule returns the Caddy module information.
func (*RateLimitMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.shield_rate_limit",
		New: func() caddy.Module { return &RateLimitMiddleware{} },
	}
}

func (m *RateLimitMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "deployment":
				if !d.AllArgs(&m.Deployment) {
					return d.ArgErr()
				}
			case "window":
				if !d.NextArg() {
					return d.ArgErr()
				}
				secs, err := parseDurationSeconds(d.Val())
				if err != nil {
					return d.Err(err.Error())
				}
				m.WindowSeconds = int(secs)
			case "limit":
				v, err := caddyfileInt(d)
				if err != nil {
					return err
				}
				m.Limit = v
			case "ban":
				if !d.NextArg() {
					return d.ArgErr()
				}
				secs, err := parseDurationSeconds(d.Val())
				if err != nil {
					return d.Err(err.Error())
				}
				m.BanSeconds = int(secs)
			case "strike_factor":
				v, err := caddyfileInt(d)
				if err != nil {
					return err
				}
				m.StrikeFactor = v
			default:
				return d.Errf("unknown subdirective %q", d.Val())
			}
		}
	}
	return nil
}

func parseRateLimitCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m RateLimitMiddleware
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

func (m *RateLimitMiddleware) Provision(ctx caddy.Context) error {
	m.logger = caddy.Log()
	registerMetrics(ctx)
	if m.Deployment == "" {
		return fmt.Errorf("shield_rate_limit: deployment is required")
	}
	if m.WindowSeconds <= 0 {
		m.WindowSeconds = 120
	}
	if m.WindowSeconds > 3600 {
		return fmt.Errorf("shield_rate_limit: window must be <= 1 hour")
	}
	if m.Limit < 10 {
		return fmt.Errorf("shield_rate_limit: limit must be >= 10")
	}
	if m.Limit > 100000 {
		return fmt.Errorf("shield_rate_limit: limit must be <= 100000")
	}
	if m.BanSeconds <= 0 {
		m.BanSeconds = 600
	}
	if m.BanSeconds > 86400 {
		return fmt.Errorf("shield_rate_limit: ban must be <= 1 day")
	}
	if m.StrikeFactor <= 0 {
		m.StrikeFactor = 3
	}
	if m.StrikeFactor > 10 {
		return fmt.Errorf("shield_rate_limit: strike_factor must be <= 10")
	}
	m.buckets = make(map[string]*rlBucket)
	m.bans = newCappedStore(65536, func() int64 { return time.Now().UnixNano() })
	return nil
}

func (m *RateLimitMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	key := clientKey(r)

	if m.bans.has(key) {
		rateRejected.WithLabelValues(m.Deployment).Inc()
		w.Header().Set("Retry-After", "60")
		http.Error(w, "temporarily banned", http.StatusForbidden)
		return nil
	}

	allowed := m.allow(key)
	if !allowed.ok {
		if allowed.banned {
			m.bans.add(key, time.Now().Add(time.Duration(m.BanSeconds)*time.Second).UnixNano())
			rateBanned.WithLabelValues(m.Deployment).Inc()
			w.Header().Set("Retry-After", fmt.Sprintf("%d", m.BanSeconds))
			http.Error(w, "temporarily banned", http.StatusForbidden)
			return nil
		}
		rateRejected.WithLabelValues(m.Deployment).Inc()
		w.Header().Set("Retry-After", fmt.Sprintf("%d", allowed.retryAfter))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintln(w, "rate limit exceeded")
		return nil
	}
	return next.ServeHTTP(w, r)
}

type rlDecision struct {
	ok         bool
	banned     bool
	retryAfter int
}

// allow records one request for the hashed key and decides its fate.
func (m *RateLimitMiddleware) allow(key string) rlDecision {
	now := time.Now().Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Cap the number of tracked keys: beyond it, untracked traffic is
	// allowed through (fail open) — an identifier flood must not exhaust
	// memory or lock out everyone.
	if len(m.buckets) > 65536 {
		for k, b := range m.buckets {
			if now-b.windowStart >= int64(m.WindowSeconds) {
				delete(m.buckets, k)
			}
		}
		if len(m.buckets) > 65536 {
			return rlDecision{ok: true}
		}
	}
	b, exists := m.buckets[key]
	if !exists || now-b.windowStart >= int64(m.WindowSeconds) {
		b = &rlBucket{windowStart: now}
		m.buckets[key] = b
	}
	b.count++
	if b.count > int64(m.StrikeFactor)*int64(m.Limit) {
		return rlDecision{banned: true}
	}
	if b.count > int64(m.Limit) {
		return rlDecision{retryAfter: m.WindowSeconds - int(now-b.windowStart)}
	}
	return rlDecision{ok: true}
}

// Interface guard.
var _ caddyhttp.MiddlewareHandler = (*RateLimitMiddleware)(nil)
