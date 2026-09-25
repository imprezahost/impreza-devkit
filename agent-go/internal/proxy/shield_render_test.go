package proxy

import (
	"strings"
	"testing"
)

func TestShieldConfigValidation(t *testing.T) {
	ok := []ShieldConfig{
		{Profile: "standard"},
		{Profile: "standard", Mode: "audit"},
		{Profile: "hardened"},
		{Profile: "hardened", Mode: "enforce"},
		{Profile: "max", Mode: "audit"},
		{Profile: "max", Mode: "enforce"},
	}
	for _, c := range ok {
		if err := c.validate(); err != nil {
			t.Fatalf("valid config rejected: %+v: %v", c, err)
		}
	}
	bad := []ShieldConfig{
		{Profile: "off"},                    // off is represented by nil, not a profile
		{Profile: ""},
		{Profile: "ultra"},
		{Profile: "hardened", Mode: "block"}, // unknown mode
		{Profile: "standard", Mode: "enforce"},
	}
	for _, c := range bad {
		if err := c.validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
	var nilCfg *ShieldConfig
	if err := nilCfg.validate(); err != nil {
		t.Fatalf("nil shield must validate: %v", err)
	}
}

func TestRenderFragmentShieldStandardAuditOnly(t *testing.T) {
	frag := renderFragment("dpl_abc123", []Route{{
		Hostname: "example.com",
		Upstream: "dpl_abc123:8080",
		Shield:   &ShieldConfig{Profile: "standard"},
	}})
	if !strings.Contains(frag, "coraza_waf {") {
		t.Fatal("waf directive missing")
	}
	if !strings.Contains(frag, "SecRuleEngine DetectionOnly") {
		t.Fatal("standard must audit only")
	}
	if !strings.Contains(frag, "SecAuditEngine Off") {
		t.Fatal("audit engine must stay off (visitor IP in Coraza audit log)")
	}
	if strings.Contains(frag, "shield_pow") || strings.Contains(frag, "shield_rate_limit") {
		t.Fatal("standard must not gate PoW or rate limit")
	}
	if !strings.Contains(frag, "load_owasp_crs") {
		t.Fatal("embedded CRS not loaded")
	}
	if !strings.Contains(frag, `setvar:tx.paranoia_level=1`) {
		t.Fatal("paranoia level 1 expected for standard")
	}
}

func TestRenderFragmentShieldHardenedIncludesPow(t *testing.T) {
	clear := renderFragment("dpl_abc123", []Route{{
		Hostname: "example.com",
		Upstream: "dpl_abc123:8080",
		Shield:   &ShieldConfig{Profile: "hardened", Mode: "audit"},
	}})
	if !strings.Contains(clear, "SecRuleEngine DetectionOnly") {
		t.Fatal("hardened without review must still audit")
	}
	if !strings.Contains(clear, "shield_pow {") || !strings.Contains(clear, "shield_rate_limit {") {
		t.Fatal("hardened must include pow + rate limit")
	}
	// Clearnet fragment: secure_cookie present, before reverse_proxy.
	if !strings.Contains(clear, "secure_cookie") {
		t.Fatal("clearnet pow must set secure_cookie")
	}
	if strings.Index(clear, "shield_pow {") > strings.Index(clear, "reverse_proxy") {
		t.Fatal("shield directives must precede reverse_proxy")
	}

	enforced := renderFragment("dpl_abc123", []Route{{
		Hostname: "example.com",
		Upstream: "dpl_abc123:8080",
		Shield:   &ShieldConfig{Profile: "hardened", Mode: "enforce"},
	}})
	if !strings.Contains(enforced, "SecRuleEngine On") {
		t.Fatal("enforce mode must turn the engine on")
	}
}

func TestRenderFragmentShieldOnionNoSecureCookie(t *testing.T) {
	frag := renderFragment("dpl_abc123", []Route{{
		OnionAddr: "abcdefghij1234567890abcdefghijklmnop.onion",
		Upstream:  "dpl_abc123:8080",
		Shield:    &ShieldConfig{Profile: "max", Mode: "enforce"},
	}})
	if !strings.Contains(frag, "shield_pow {") {
		t.Fatal("pow missing on onion route")
	}
	if strings.Contains(frag, "secure_cookie") {
		t.Fatal("onion routes must NOT set the Secure cookie attribute (plain HTTP)")
	}
	if !strings.Contains(frag, `setvar:tx.paranoia_level=2`) {
		t.Fatal("max profile must set paranoia level 2")
	}
}

func TestRenderFragmentShieldOffEmitsNothing(t *testing.T) {
	frag := renderFragment("dpl_abc123", []Route{{
		Hostname: "example.com",
		Upstream: "dpl_abc123:8080",
	}})
	for _, token := range []string{"coraza_waf", "shield_pow", "shield_rate_limit"} {
		if strings.Contains(frag, token) {
			t.Fatalf("%s emitted for profile off", token)
		}
	}
}

func TestShieldGlobalOptions(t *testing.T) {
	plain := shieldGlobalOptions(false)
	if !strings.Contains(plain, "order coraza_waf first") {
		t.Fatal("directive order missing")
	}
	if strings.Contains(plain, "metrics") {
		t.Fatal("metrics listener enabled without shield routes")
	}
	withShield := shieldGlobalOptions(true)
	if !strings.Contains(withShield, "metrics") {
		t.Fatal("metrics listener not enabled with shield routes")
	}
}

func TestFragmentUsesShield(t *testing.T) {
	if fragmentUsesShield("# plain\nexample.com {\n reverse_proxy x\n}\n") {
		t.Fatal("plain fragment detected as shield")
	}
	shield := renderFragment("dpl_abc123", []Route{{
		Hostname: "example.com",
		Upstream: "x:1",
		Shield:   &ShieldConfig{Profile: "standard"},
	}})
	if !fragmentUsesShield(shield) {
		t.Fatal("shield fragment not detected")
	}
}

func TestRenderFragmentAlwaysCountsProxyMetrics(t *testing.T) {
	// Profile off — counters still render: proxy metrics are not part of the
	// Shield tiers, they are the observability baseline.
	frag := renderFragment("dpl_abc123", []Route{{
		Hostname: "example.com",
		Upstream: "dpl_abc123:8080",
	}})
	if !strings.Contains(frag, "proxy_metrics {") {
		t.Fatal("proxy_metrics missing for a routed deployment")
	}
	if !strings.Contains(frag, "deployment dpl_abc123") {
		t.Fatal("proxy_metrics not labeled by deployment")
	}
	if strings.Index(frag, "proxy_metrics {") > strings.Index(frag, "reverse_proxy") {
		t.Fatal("proxy_metrics must wrap reverse_proxy")
	}
	// The counting middleware has no identity-bearing label surface.
	for _, leak := range []string{"remote", "client_ip", "path", "user_agent"} {
		if strings.Contains(frag, leak) {
			t.Fatalf("proxy_metrics fragment leaks %q", leak)
		}
	}
}

func TestRenderFragmentTLSNoneIsPlainHTTP(t *testing.T) {
	// Regression (live battery 20260925-d02a7f): TLSMode none must emit the
	// http:// site address — a bare hostname 308-redirects to auto-HTTPS.
	frag := renderFragment("dpl_abc123", []Route{{
		Hostname: "plain.example.com",
		Upstream: "dpl_abc123:8080",
		TLSMode:  "none",
	}})
	if !strings.Contains(frag, "http://plain.example.com {\n") {
		t.Fatalf("TLSMode none must use the http:// site address:\n%s", frag)
	}
	if strings.Contains(frag, "  tls ") {
		t.Fatalf("TLSMode none must not emit a tls directive:\n%s", frag)
	}
	// Plain-HTTP clearnet: shield cookies without the Secure attribute.
	shielded := renderFragment("dpl_abc123", []Route{{
		Hostname: "plain.example.com",
		Upstream: "dpl_abc123:8080",
		TLSMode:  "none",
		Shield:   &ShieldConfig{Profile: "hardened"},
	}})
	if strings.Contains(shielded, "secure_cookie") {
		t.Fatal("secure_cookie must not be set on plain-HTTP clearnet")
	}
	// ...while a TLS route keeps it.
	tlsRoute := renderFragment("dpl_abc123", []Route{{
		Hostname: "secure.example.com",
		Upstream: "dpl_abc123:8080",
		Shield:   &ShieldConfig{Profile: "hardened"},
	}})
	if !strings.Contains(tlsRoute, "secure_cookie") {
		t.Fatal("secure_cookie missing on a TLS clearnet route")
	}
}
