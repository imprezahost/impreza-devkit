package client

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestShieldControlsBounds(t *testing.T) {
	now := time.Now()
	i := func(v int) *int { return &v }
	yes := []ShieldControls{
		{}, {PowPaths: []string{"/", "/api", "/login", "/" + strings.Repeat("a", 255)}},
		{RateLimitRPM: i(30)}, {RateLimitRPM: i(31)}, {RateLimitRPM: i(5999)}, {RateLimitRPM: i(6000)},
		{PowDifficulty: i(2)}, {PowDifficulty: i(3)}, {PowDifficulty: i(4)},
		{TrustedSources: []string{"198.18.0.0/16", "192.0.2.1/32", "2001:db8::/48", "2001:db8::1/128"}},
		{AttackExpiresAt: 1}, {AttackExpiresAt: now.Unix() + 3600}, {AttackExpiresAt: now.Unix() + 86400},
	}
	for n, s := range yes {
		if err := ValidateShieldControls(&s, now); err != nil {
			t.Fatalf("valid %d: %v", n, err)
		}
	}
	no := []ShieldControls{
		{PowPaths: []string{""}}, {PowPaths: []string{"missing-slash"}}, {PowPaths: []string{"/" + strings.Repeat("a", 256)}},
		{PowPaths: []string{"/dup", "/dup"}},
		{RateLimitRPM: i(29)}, {RateLimitRPM: i(6001)}, {RateLimitRPM: i(0)},
		{PowDifficulty: i(1)}, {PowDifficulty: i(5)}, {PowDifficulty: i(0)},
		{TrustedSources: []string{"0.0.0.0/0"}}, {TrustedSources: []string{"::/0"}},
		{TrustedSources: []string{"198.18.0.0/15"}}, {TrustedSources: []string{"2001:db8::/47"}},
		{TrustedSources: []string{"198.18.0.1/16"}}, {TrustedSources: []string{"2001:db8::1/48"}},
		{TrustedSources: []string{"::ffff:192.0.2.1/128"}}, {TrustedSources: []string{"::ffff:c000:201/128"}},
		{TrustedSources: []string{"192.0.2.0/24", "192.0.2.0/24"}},
		{AttackExpiresAt: -1}, {AttackExpiresAt: now.Unix() + 86701},
	}
	for _, bad := range []string{"/x\ny", "/x\"", "/x}", "/x#", "/x y", "/xＡ", "/x\\y", "/x`y"} {
		no = append(no, ShieldControls{PowPaths: []string{bad}})
	}
	paths, sources := []string{}, []string{}
	for n := 0; n < 11; n++ {
		paths = append(paths, "/"+strings.Repeat("a", n))
		sources = append(sources, fmt.Sprintf("192.0.2.%d/32", n))
	}
	if _, err := ParseShieldTrustedSources(sources[:10]); err != nil {
		t.Fatal(err)
	}
	if err := ValidateShieldPowPaths(paths[:10]); err != nil {
		t.Fatal(err)
	}
	no = append(no, ShieldControls{PowPaths: paths}, ShieldControls{TrustedSources: sources})
	for n, s := range no {
		if err := ValidateShieldControls(&s, now); err == nil {
			t.Fatalf("invalid %d accepted", n)
		}
	}
	for _, raw := range []string{`{"pow_paths":[1]}`, `{"rate_limit_rpm":"30"}`, `{"pow_difficulty":2.5}`, `{"attack_expires_at":"0"}`, `{"raw_directive":"deny"}`, `{"under_attack":{"enabled":true}}`} {
		var s ShieldControls
		if json.Unmarshal([]byte(raw), &s) == nil {
			t.Fatalf("invalid typed wire accepted: %s", raw)
		}
	}
}
