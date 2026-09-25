package caddyshield

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)


// Regression (live battery 20260925-d02a7f): duration subdirectives read the
// subdirective NAME instead of consuming their argument, so every generated
// fragment failed `caddy reload` with `invalid duration "window"`. These
// tests drive the REAL Caddyfile dispenser path the proxy renders into.
func TestPowCaddyfileParsing(t *testing.T) {
	d := caddyfile.NewTestDispenser(`shield_pow {
		deployment dpl_aaaa111122223333
		difficulty 3
		max_difficulty 5
		adaptive_threshold 80
		cookie_ttl 1h
		challenge_ttl 3m
		secure_cookie
	}`)
	var m PowMiddleware
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Deployment != "dpl_aaaa111122223333" || m.Difficulty != 3 || m.MaxDifficulty != 5 {
		t.Fatalf("ints wrong: %+v", &m)
	}
	if m.AdaptiveThreshold != 80 {
		t.Fatalf("adaptive threshold wrong: %+v", &m)
	}
	if m.CookieTTLSeconds != 3600 || m.ChallengeTTLSeconds != 180 {
		t.Fatalf("durations wrong: %+v", &m)
	}
	if !m.SecureCookie {
		t.Fatal("secure_cookie lost")
	}
}

func TestRateLimitCaddyfileParsing(t *testing.T) {
	d := caddyfile.NewTestDispenser(`shield_rate_limit {
		deployment dpl_aaaa111122223333
		window 2m
		limit 240
		ban 10m
		strike_factor 3
	}`)
	var m RateLimitMiddleware
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Deployment != "dpl_aaaa111122223333" || m.WindowSeconds != 120 {
		t.Fatalf("window wrong: %+v", &m)
	}
	if m.Limit != 240 || m.BanSeconds != 600 || m.StrikeFactor != 3 {
		t.Fatalf("limits wrong: %+v", &m)
	}
}

func TestProxyMetricsCaddyfileParsing(t *testing.T) {
	d := caddyfile.NewTestDispenser(`proxy_metrics {
		deployment dpl_aaaa111122223333
	}`)
	var m ProxyMetricsMiddleware
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Deployment != "dpl_aaaa111122223333" {
		t.Fatalf("deployment wrong: %+v", &m)
	}
	// Unknown subdirectives refuse loudly.
	d2 := caddyfile.NewTestDispenser(`proxy_metrics {
		deployment dpl_x
		bogus 1
	}`)
	var m2 ProxyMetricsMiddleware
	if err := m2.UnmarshalCaddyfile(d2); err == nil {
		t.Fatal("unknown subdirective accepted")
	}
}

// The exact fragment shapes the agent renderer emits must all parse.
func TestRendererFragmentShapesParse(t *testing.T) {
	shapes := []string{
		`shield_rate_limit {
			deployment dpl_aaaa111122223333
			window 2m
			limit 240
			ban 10m
		}`,
		`shield_pow {
			deployment dpl_aaaa111122223333
			difficulty 3
			max_difficulty 5
			adaptive_threshold 120
			cookie_ttl 1h
			challenge_ttl 3m
			secure_cookie
		}`,
		`shield_pow {
			deployment dpl_aaaa111122223333
			difficulty 4
			max_difficulty 6
			adaptive_threshold 60
			cookie_ttl 30m
			challenge_ttl 2m
		}`,
		`proxy_metrics {
			deployment dpl_aaaa111122223333
		}`,
	}
	for i, shape := range shapes {
		switch {
		case strings.HasPrefix(shape, "shield_rate_limit"):
			var m RateLimitMiddleware
			if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(shape)); err != nil {
				t.Fatalf("shape %d: %v", i, err)
			}
			if err := m.Provision(caddy.Context{}); err != nil {
				t.Fatalf("shape %d provision: %v", i, err)
			}
		case strings.HasPrefix(shape, "shield_pow"):
			var m PowMiddleware
			if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(shape)); err != nil {
				t.Fatalf("shape %d: %v", i, err)
			}
			if err := m.Provision(caddy.Context{}); err != nil {
				t.Fatalf("shape %d provision: %v", i, err)
			}
		default:
			var m ProxyMetricsMiddleware
			if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(shape)); err != nil {
				t.Fatalf("shape %d: %v", i, err)
			}
			if err := m.Provision(caddy.Context{}); err != nil {
				t.Fatalf("shape %d provision: %v", i, err)
			}
		}
	}
}
