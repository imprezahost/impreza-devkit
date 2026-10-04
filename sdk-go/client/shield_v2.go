package client

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
)

const ShieldV2Protocol = "shield-v2"
const MaxShieldExclusions = 20

// Static metadata from the pinned, embedded CRS 4.25.0. No expanded rule
// messages or matched values are used for telemetry or descriptions.
//
//go:embed shield_rules.json
var shieldRulesFS embed.FS

type ShieldRuleMetadata struct {
	Category    string `json:"category"`
	Description string `json:"description"`
	Blocking    bool   `json:"blocking"`
	Detection   bool   `json:"detection"`
}

var shieldRuleIndex = func() map[int]ShieldRuleMetadata {
	b, err := shieldRulesFS.ReadFile("shield_rules.json")
	if err != nil {
		panic(err)
	}
	var document struct {
		Rules map[int]ShieldRuleMetadata `json:"rules"`
	}
	if err := json.Unmarshal(b, &document); err != nil {
		panic(err)
	}
	return document.Rules
}()

func ShieldRule(id int) (ShieldRuleMetadata, bool) { m, ok := shieldRuleIndex[id]; return m, ok }

var shieldPrefixPattern = regexp.MustCompile(`^/[A-Za-z0-9/_.-]*$`)
var shieldDeploymentPattern = regexp.MustCompile(`^dpl_[a-f0-9]{16}(?:[a-f0-9]{8})?$`)

func ValidateShieldDeployment(id string) error {
	if !shieldDeploymentPattern.MatchString(id) {
		return fmt.Errorf("invalid Shield deployment id")
	}
	return nil
}

type ShieldExclusion struct {
	RuleID     int    `json:"rule_id"`
	PathPrefix string `json:"path_prefix,omitempty"`
}

func ValidateShieldExclusions(exclusions []ShieldExclusion) error {
	if len(exclusions) > MaxShieldExclusions {
		return fmt.Errorf("at most 20 Shield exclusions are allowed")
	}
	seen := map[ShieldExclusion]bool{}
	for _, x := range exclusions {
		metadata, ok := ShieldRule(x.RuleID)
		if !ok {
			return fmt.Errorf("rule_id must belong to the embedded OWASP CRS")
		}
		if !metadata.Detection {
			return fmt.Errorf("Only detection rules from the embedded OWASP CRS can be excluded.")
		}
		if x.PathPrefix != "" && (len(x.PathPrefix) > 256 || !shieldPrefixPattern.MatchString(x.PathPrefix)) {
			return fmt.Errorf("path_prefix must be an ASCII path prefix beginning with / (maximum 256 characters)")
		}
		if seen[x] {
			return fmt.Errorf("duplicate Shield exclusion")
		}
		seen[x] = true
	}
	return nil
}

// Counters only. Outcomes are once per request and findings once per rule per
// request. Multiple matching rules must never inflate the request total.
type ShieldWAFMetrics struct {
	WouldBlock int64                  `json:"would_block"`
	Blocked    int64                  `json:"blocked"`
	Rules      []ShieldWAFRuleMetrics `json:"rules,omitempty"`
}
type ShieldWAFRuleMetrics struct {
	RuleID   int    `json:"rule_id"`
	Category string `json:"category"`
	Outcome  string `json:"outcome"` // matched | would_block | blocked
	Count    int64  `json:"count"`
}
