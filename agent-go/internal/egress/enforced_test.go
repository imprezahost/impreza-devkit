package egress

// On a host whose Docker runs with "iptables": false the
// host INPUT half and the v6 half still apply while the v4 FORWARD half cannot
// (no DOCKER-USER), as measured on a real host. Enforced must follow the v4 FORWARD
// half only, or every deploy there fails closed.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnforcedFollowsTheV4ForwardHalf(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         bool
	}{
		{"no status file", "", false},
		{"iptables-false host: host and v6 halves applied, v4 FORWARD not",
			`{"v4":{"applied":false,"error":"egress parent chain unavailable; docker may not be running yet","last_attempt":"","rules":0,"resolvers":1},"v6":{"applied":true,"last_attempt":"","rules":9,"resolvers":0},"v4_host":{"applied":true,"last_attempt":"","rules":4,"resolvers":0},"v6_host":{"applied":true,"last_attempt":"","rules":4,"resolvers":0}}`, false},
		{"normal host: v4 FORWARD applied", `{"v4":{"applied":true,"last_attempt":"","rules":12,"resolvers":1}}`, true},
	} {
		dir := t.TempDir()
		if tc.status != "" {
			if err := os.WriteFile(filepath.Join(dir, "egress.json"), []byte(tc.status), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := Enforced(dir); got != tc.want {
			t.Errorf("%s: Enforced=%v, want %v", tc.name, got, tc.want)
		}
	}
}
