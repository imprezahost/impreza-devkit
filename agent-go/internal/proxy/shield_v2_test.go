package proxy

import (
	"context"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShieldV2RenderBoundary(t *testing.T) {
	sc := &ShieldConfig{Profile: "standard", Mode: "audit", Protocol: sdkclient.ShieldV2Protocol, Exclusions: []sdkclient.ShieldExclusion{{RuleID: 942100}, {RuleID: 941100, PathPrefix: "/api/form"}}}
	r := Route{Hostname: "shield.example.invalid", Upstream: "app:80", TLSMode: "none", Shield: sc}
	err := sc.validate()
	fragment := renderFragment("dpl_0123456789abcdef", []Route{r})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fragment, "SecRuleRemoveById 942100") || !strings.Contains(fragment, `"@beginsWith /api/form"`) {
		t.Fatal(fragment)
	}
	if strings.Index(fragment, "ctl:ruleRemoveById=941100") > strings.Index(fragment, "Include @owasp_crs/*.conf") {
		t.Fatal("conditional exclusion must run before CRS")
	}
	c := New(t.TempDir(), slog.Default())
	reject := func() {
		t.Helper()
		err := c.ApplyDeploymentRoutes(context.Background(), "dpl_0123456789abcdef", []Route{r})
		if err == nil {
			t.Fatal("unsafe policy accepted")
		}
		if _, err := os.Stat(filepath.Join(c.StateDir, "deployments", "dpl_0123456789abcdef.caddy")); !os.IsNotExist(err) {
			t.Fatal("unsafe fragment reached disk before rejection")
		}
	}
	for _, p := range []string{"/x\nSecRuleEngine Off", "/x\"", "/x}", "/x#", "/x y", "/xＡ", "x", "/a?", "/a\\b"} {
		sc.Exclusions = []sdkclient.ShieldExclusion{{RuleID: 942100, PathPrefix: p}}
		reject()
	}
	sc.Exclusions = []sdkclient.ShieldExclusion{{RuleID: 999999}}
	reject()
	sc.Exclusions = []sdkclient.ShieldExclusion{{RuleID: 942100}}
	sc.Protocol = ""
	reject()
}
func TestShieldWAFCounterDeltas(t *testing.T) {
	text := `impreza_shield_waf_requests_total{deployment="dpl_0123456789abcdef",outcome="would_block"} 2
impreza_shield_waf_rules_total{deployment="dpl_0123456789abcdef",rule_id="942100",category="SQLi",outcome="would_block"} 2
impreza_shield_waf_rules_total{deployment="dpl_0123456789abcdef",rule_id="941100",category="XSS",outcome="would_block"} 1
impreza_shield_waf_rules_total{deployment="dpl_0123456789abcdef",rule_id="942100",category="SQLi",outcome="would_block",ip="<VISITOR_ID>"} 100
impreza_shield_waf_rules_total{deployment="dpl_0123456789abcdef",rule_id="999999",category="SQLi",outcome="would_block"} 100
`
	current := parseExposition(text)
	delta := aggregateDeltas(diffSamples(nil, current))
	m := delta["dpl_0123456789abcdef"]
	if m == nil || m.WAF == nil || m.WAF.WouldBlock != 2 || len(m.WAF.Rules) != 2 {
		t.Fatalf("bad WAF delta %+v", m)
	}
	var count int64
	for _, rule := range m.WAF.Rules {
		count += rule.Count
	}
	if count != 3 {
		t.Fatal("identity-bearing or invalid rule series entered the numeric delta")
	}
	if len(diffSamples(current, current)) != 0 {
		t.Fatal("unchanged counters replayed")
	}
	reset := aggregateDeltas(diffSamples(current, parseExposition(strings.ReplaceAll(text, "} 2", "} 1"))))
	if reset["dpl_0123456789abcdef"].WAF.WouldBlock != 1 {
		t.Fatal("counter reset not accounted")
	}
}
