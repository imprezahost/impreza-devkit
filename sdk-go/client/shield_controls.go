package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"time"
)

const ShieldControlsProtocol = "shield-v2-controls"
const ShieldMinRateRPM = 30
const ShieldMaxRateRPM = 6000
const ShieldMinDifficulty = 2
const ShieldMaxDifficulty = 4
const ShieldAttackDifficulty = 4 // The max profile's initial difficulty.
const ShieldMaxControlItems = 10

// ShieldControls carries base settings plus an absolute, retry-stable deadline.
// Caddy restores the base settings locally when this persisted deadline expires.
// TrustedSources is customer configuration; never log or export its contents.
type ShieldControls struct {
	PowPaths        []string `json:"pow_paths"`
	RateLimitRPM    *int     `json:"rate_limit_rpm"`
	PowDifficulty   *int     `json:"pow_difficulty"`
	TrustedSources  []string `json:"trusted_sources"`
	AttackExpiresAt int64    `json:"attack_expires_at"`
}

// A corrupt or newer nested policy must not silently lose a protection.
func (s *ShieldControls) UnmarshalJSON(raw []byte) error {
	type wire ShieldControls
	var v wire
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return fmt.Errorf("invalid Shield controls")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("invalid Shield controls")
	}
	*s = ShieldControls(v)
	return nil
}

var shieldPowPath = regexp.MustCompile(`^/[A-Za-z0-9/_.-]*$`)

func ValidateShieldPowPaths(paths []string) error {
	if len(paths) > ShieldMaxControlItems {
		return fmt.Errorf("Shield PoW accepts at most 10 prefixes")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if len(p) > 256 || !shieldPowPath.MatchString(p) {
			return fmt.Errorf("invalid Shield PoW path prefix")
		}
		if seen[p] {
			return fmt.Errorf("duplicate Shield PoW path prefix")
		}
		seen[p] = true
	}
	return nil
}

func ParseShieldTrustedSources(sources []string) ([]netip.Prefix, error) {
	if len(sources) > ShieldMaxControlItems {
		return nil, fmt.Errorf("Shield accepts at most 10 trusted CIDRs")
	}
	out := make([]netip.Prefix, 0, len(sources))
	seen := map[netip.Prefix]bool{}
	for _, raw := range sources {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() || prefix != prefix.Masked() {
			return nil, fmt.Errorf("invalid Shield trusted CIDR")
		}
		floor := 48
		if prefix.Addr().Is4() {
			floor = 16
		}
		if prefix.Bits() < floor {
			return nil, fmt.Errorf("Shield trusted CIDR is too broad")
		}
		if seen[prefix] {
			return nil, fmt.Errorf("duplicate Shield trusted CIDR")
		}
		seen[prefix] = true
		out = append(out, prefix)
	}
	return out, nil
}

func ValidateShieldControls(s *ShieldControls, now time.Time) error {
	if s == nil {
		return nil
	}
	if err := ValidateShieldPowPaths(s.PowPaths); err != nil {
		return err
	}
	if s.RateLimitRPM != nil && (*s.RateLimitRPM < ShieldMinRateRPM || *s.RateLimitRPM > ShieldMaxRateRPM) {
		return fmt.Errorf("Shield rate_limit_rpm must be 30-6000")
	}
	if s.PowDifficulty != nil && (*s.PowDifficulty < ShieldMinDifficulty || *s.PowDifficulty > ShieldMaxDifficulty) {
		return fmt.Errorf("Shield pow_difficulty must be 2-4")
	}
	if _, err := ParseShieldTrustedSources(s.TrustedSources); err != nil {
		return err
	}
	// Old absolute deadlines remain valid: delayed commands and retries must not rearm them.
	// Five minutes tolerate bounded clock skew; the API duration remains strictly 1-24h.
	if s.AttackExpiresAt < 0 || s.AttackExpiresAt > now.Unix()+86400+300 {
		return fmt.Errorf("invalid Shield attack deadline")
	}
	return nil
}
