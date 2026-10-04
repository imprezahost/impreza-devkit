package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The controls go through their JSON wire form and the real route entry
// (ApplyDeploymentRoutes), so the test fails whenever they are ignored.
func TestShieldControlsWire(t *testing.T) {
	type testCase struct {
		id, controls, expected string
		valid                  bool
	}
	cases := []testCase{
		{"paths", `{"pow_paths":["/admin/"]}`, "path_prefix /admin/", true},
		{"RPM floor", `{"rate_limit_rpm":30}`, "limit 30", true},
		{"RPM ceiling", `{"rate_limit_rpm":6000}`, "limit 6000", true},
		{"difficulty floor", `{"pow_difficulty":2}`, "difficulty 2", true},
		{"difficulty ceiling", `{"pow_difficulty":4}`, "difficulty 4", true},
		{"CIDR v4 floor", `{"trusted_sources":["198.18.0.0/16"]}`, "trusted_source 198.18.0.0/16", true},
		{"CIDR v6 floor", `{"trusted_sources":["2001:db8::/48"]}`, "trusted_source 2001:db8::/48", true},
		{"deadline valid", fmt.Sprintf(`{"attack_expires_at":%d}`, time.Now().Unix()+3600), "attack_until ", true},
		{"deadline expired replay", `{"attack_expires_at":1}`, "attack_until 1", true},
		{"path injection", `{"pow_paths":["/admin\n}"]}`, "", false},
		{"path 257", fmt.Sprintf(`{"pow_paths":["/%s"]}`, strings.Repeat("a", 256)), "", false},
		{"RPM below", `{"rate_limit_rpm":29}`, "", false},
		{"RPM above", `{"rate_limit_rpm":6001}`, "", false},
		{"difficulty below", `{"pow_difficulty":1}`, "", false},
		{"difficulty above", `{"pow_difficulty":5}`, "", false},
		{"CIDR wide v4", `{"trusted_sources":["198.18.0.0/15"]}`, "", false},
		{"CIDR wide v6", `{"trusted_sources":["2001:db8::/47"]}`, "", false},
		{"CIDR world v4", `{"trusted_sources":["0.0.0.0/0"]}`, "", false},
		{"CIDR world v6", `{"trusted_sources":["::/0"]}`, "", false},
		{"CIDR host bits", `{"trusted_sources":["192.0.2.1/24"]}`, "", false},
		{"CIDR mapped", `{"trusted_sources":["::ffff:192.0.2.1/128"]}`, "", false},
		{"deadline negative", `{"attack_expires_at":-1}`, "", false},
		{"deadline over ceiling", fmt.Sprintf(`{"attack_expires_at":%d}`, time.Now().Unix()+90000), "", false},
		{"typed RPM", `{"rate_limit_rpm":"30"}`, "", false},
		{"raw deadline duration", `{"under_attack":{"enabled":true,"duration_hours":1}}`, "", false},
	}
	paths, sources := []string{}, []string{}
	for n := 0; n < 11; n++ {
		paths = append(paths, fmt.Sprintf("/p%d", n))
		sources = append(sources, fmt.Sprintf("192.0.2.%d/32", n))
	}
	for _, count := range []int{10, 11} {
		p, _ := json.Marshal(paths[:count])
		c, _ := json.Marshal(sources[:count])
		cases = append(cases, testCase{fmt.Sprintf("path count %d", count), `{"pow_paths":` + string(p) + `}`, "path_prefix /p0", count == 10})
		cases = append(cases, testCase{fmt.Sprintf("CIDR count %d", count), `{"trusted_sources":` + string(c) + `}`, "trusted_source 192.0.2.0/32", count == 10})
	}
	cases = append(cases, testCase{"path at 256", fmt.Sprintf(`{"pow_paths":["/%s"]}`, strings.Repeat("a", 255)), "path_prefix /" + strings.Repeat("a", 255), true})
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			sc := &ShieldConfig{}
			err := json.Unmarshal([]byte(`{"profile":"hardened","mode":"audit","protocol":"shield-v2","controls":`+tc.controls+`}`), sc)
			c := &Caddy{StateDir: t.TempDir(), switchReload: func(context.Context) error { return nil }}
			if err == nil {
				err = c.ApplyDeploymentRoutes(context.Background(), "dpl_controls", []Route{{Hostname: "controls.test", Upstream: "app:80", TLSMode: "none", Shield: sc}})
			}
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				data, e := os.ReadFile(filepath.Join(c.StateDir, "deployments", "dpl_controls.caddy"))
				if e != nil {
					t.Fatal(e)
				}
				if !strings.Contains(string(data), tc.expected) {
					t.Fatalf("wire control not applied: %s", tc.id)
				}
			} else {
				if err == nil {
					t.Fatalf("invalid control passed actual route entry: %s", tc.id)
				}
				if _, e := os.Stat(filepath.Join(c.StateDir, "deployments", "dpl_controls.caddy")); !os.IsNotExist(e) {
					t.Fatal("rejected wire modified routing state")
				}
			}
		})
	}
}
