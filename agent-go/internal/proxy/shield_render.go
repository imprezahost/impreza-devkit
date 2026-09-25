// Shield fragment rendering (capability shield-v1).
//
// One ShieldConfig per deployment, stamped onto every route of that
// deployment. Profiles (maintainer decision 7.4):
//
//   - standard: Coraza + embedded OWASP CRS in DetectionOnly (audit) —
//     never blocks; included tier, default for NEW deployments only.
//   - hardened: WAF with enforce after false-positive review + PoW gate +
//     rate limit (add-on).
//   - max: hardened with CRS paranoia level 2, harder PoW and tighter
//     limits (add-on).
//
// Privacy contract: SecAuditEngine is OFF — Coraza's JSON audit log embeds
// the visitor IP, so it must never be enabled on this platform. Telemetry
// is aggregate Prometheus counters only. No client IP is logged by the
// proxy stack at all.
package proxy

import (
	"fmt"
	"strings"
)

// Shield profiles and modes accepted in a Route.
const (
	shieldProfileStandard  = "standard"
	shieldProfileHardened  = "hardened"
	shieldProfileMax       = "max"
	shieldModeAudit        = "audit"
	shieldModeEnforce      = "enforce"
)

// ShieldConfig is the per-deployment Impreza Shield policy.
type ShieldConfig struct {
	// Profile selects the protection bundle: standard | hardened | max.
	Profile string
	// Mode governs the WAF engine: audit (DetectionOnly) or enforce
	// (blocking). The control plane only sends enforce after the tenant's
	// false-positive review; the agent double-checks here.
	Mode string
}

// validate refuses malformed shield policies instead of emitting fragments
// a reload would reject (which would break every route on the host).
func (s *ShieldConfig) validate() error {
	if s == nil {
		return nil
	}
	switch s.Profile {
	case shieldProfileStandard, shieldProfileHardened, shieldProfileMax:
	default:
		return fmt.Errorf("shield profile %q is not one of standard|hardened|max", s.Profile)
	}
	switch s.Mode {
	case "", shieldModeAudit, shieldModeEnforce:
	default:
		return fmt.Errorf("shield mode %q is not one of audit|enforce", s.Mode)
	}
	// The standard profile is audit-only by decision 7.4; enforce belongs to
	// hardened/max only.
	if s.Profile == shieldProfileStandard && s.Mode == shieldModeEnforce {
		return fmt.Errorf("shield profile standard never enforces (audit-only by design)")
	}
	return nil
}

// wafEngineMode maps mode → SecRuleEngine value. Empty mode defaults to
// audit — fail-safe, never fail-blocking.
func (s *ShieldConfig) wafEngineMode() string {
	if s != nil && s.Mode == shieldModeEnforce {
		return "On"
	}
	return "DetectionOnly"
}

// paranoiaLevel: CRS PL1 for standard/hardened, PL2 for max.
func (s *ShieldConfig) paranoiaLevel() int {
	if s != nil && s.Profile == shieldProfileMax {
		return 2
	}
	return 1
}

// writeShieldDirectives emits the proxy-side protection stack for one site
// block. secureCookie MUST be false for onion blocks (plain HTTP — the
// Secure attribute would make browsers drop the cookie) and true for
// clearnet. deploymentID only labels metrics counters.
func writeShieldDirectives(sb *strings.Builder, deploymentID string, sc *ShieldConfig, secureCookie bool) {
	if sc == nil {
		return
	}

	// WAF: Coraza with the CRS embedded in the image's Go module graph.
	fmt.Fprintf(sb, "  coraza_waf {\n")
	fmt.Fprintf(sb, "    load_owasp_crs\n")
	fmt.Fprintf(sb, "    directives `\n")
	fmt.Fprintf(sb, "Include @coraza.conf-recommended\n")
	fmt.Fprintf(sb, "Include @crs-setup.conf.example\n")
	fmt.Fprintf(sb, "Include @owasp_crs/*.conf\n")
	fmt.Fprintf(sb, "SecRuleEngine %s\n", sc.wafEngineMode())
	fmt.Fprintf(sb, "SecAction \"id:910500,phase:1,pass,nolog,setvar:tx.paranoia_level=%d\"\n", sc.paranoiaLevel())
	// Privacy: the Coraza audit log embeds the client IP; it stays off.
	fmt.Fprintf(sb, "SecAuditEngine Off\n")
	fmt.Fprintf(sb, "    `\n")
	fmt.Fprintf(sb, "  }\n")

	if sc.Profile == shieldProfileStandard {
		return
	}

	// PoW gate + rate limit only on the add-on tiers.
	difficulty, maxDifficulty, threshold := 3, 5, 120
	cookieTTL, challengeTTL := "1h", "3m"
	window, limit, ban := "2m", 240, "10m"
	if sc.Profile == shieldProfileMax {
		difficulty, maxDifficulty, threshold = 4, 6, 60
		cookieTTL, challengeTTL = "30m", "2m"
		window, limit, ban = "1m", 120, "15m"
	}
	fmt.Fprintf(sb, "  shield_rate_limit {\n")
	fmt.Fprintf(sb, "    deployment %s\n", deploymentID)
	fmt.Fprintf(sb, "    window %s\n", window)
	fmt.Fprintf(sb, "    limit %d\n", limit)
	fmt.Fprintf(sb, "    ban %s\n", ban)
	fmt.Fprintf(sb, "  }\n")
	fmt.Fprintf(sb, "  shield_pow {\n")
	fmt.Fprintf(sb, "    deployment %s\n", deploymentID)
	fmt.Fprintf(sb, "    difficulty %d\n", difficulty)
	fmt.Fprintf(sb, "    max_difficulty %d\n", maxDifficulty)
	fmt.Fprintf(sb, "    adaptive_threshold %d\n", threshold)
	fmt.Fprintf(sb, "    cookie_ttl %s\n", cookieTTL)
	fmt.Fprintf(sb, "    challenge_ttl %s\n", challengeTTL)
	if secureCookie {
		fmt.Fprintf(sb, "    secure_cookie\n")
	}
	fmt.Fprintf(sb, "  }\n")
}

// writeProxyMetrics emits the per-deployment request counters middleware.
// Present for EVERY routed deployment on the new agent image —
// the counters carry the deployment id only, never a visitor identity, and
// the control plane reads them only from agents that announce
// proxy-metrics-v1.
func writeProxyMetrics(sb *strings.Builder, deploymentID string) {
	fmt.Fprintf(sb, "  proxy_metrics {\n    deployment %s\n  }\n", deploymentID)
}

// shieldGlobalOptions returns the global Caddyfile options the new agent
// requires. coraza_waf does not self-register a directive order, so it must
// be ordered first; the admin metrics listener (loopback inside the proxy
// container) is enabled whenever any deployment is routed so the proxy and
// shield counters become scrapable.
func shieldGlobalOptions(anyMetrics bool) string {
	var sb strings.Builder
	sb.WriteString("{\n    order coraza_waf first\n")
	// coraza-caddy logs every rule match with [client "<ip>"] through the
	// http.handlers.waf zap logger. The Shield privacy contract forbids any
	// visitor identity in proxy logs (audit engine stays off for the same
	// reason), so the namespace is silenced wholesale. WAF troubleshooting
	// on the host means temporarily re-enabling it WITH that knowledge.
	sb.WriteString("    log {\n        exclude http.handlers.waf\n    }\n")
	if anyMetrics {
		// Global metrics option (Caddy >= 2.9): the nested servers{} form
		// is deprecated and warns on every reload.
		sb.WriteString("    metrics\n")
	}
	sb.WriteString("}\n\n")
	return sb.String()
}

// fragmentUsesShield reports whether a rendered fragment references the
// shield stack (used to decide the metrics listener).
func fragmentUsesShield(fragment string) bool {
	return strings.Contains(fragment, "coraza_waf") ||
		strings.Contains(fragment, "shield_pow") ||
		strings.Contains(fragment, "shield_rate_limit")
}
