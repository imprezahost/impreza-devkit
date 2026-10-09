package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFailoverWorkflowRequests(t *testing.T) {
	ctx := context.Background()
	dep := "dpl_" + strings.Repeat("a", 16)
	target := "dpl_" + strings.Repeat("b", 24)
	id := "fov_" + strings.Repeat("c", 24)
	failback := "fbk_" + strings.Repeat("e", 24)
	drill := "fdr_" + strings.Repeat("a", 24)
	interval := 60
	digest := strings.Repeat("d", 64)
	sync := FailoverSync{"bkp_" + strings.Repeat("e", 16), "bkp_" + strings.Repeat("f", 16), "cmd_" + strings.Repeat("a", 16)}
	confirm := FailoverConfirmation{digest, true}
	base := "/v1/platform/deployments/custom/" + dep
	cases := []struct {
		name, method, path string
		body               any
		call               func(*Client) error
	}{
		{"pair", "POST", base + "/failover-standby", map[string]string{"mode": "cold", "target_deployment_id": target}, func(c *Client) error { _, e := c.PlatformPairColdStandby(ctx, dep, target); return e }},
		{"standby", "GET", base + "/failover-standby", nil, func(c *Client) error { _, e := c.PlatformGetFailoverStandby(ctx, dep); return e }},
		{"sync", "POST", base + "/failover-standby/confirm-sync", sync, func(c *Client) error { _, e := c.PlatformConfirmFailoverSync(ctx, dep, sync); return e }},
		{"prepare", "POST", base + "/prepare-failover", struct{}{}, func(c *Client) error { _, e := c.PlatformPrepareFailover(ctx, dep); return e }},
		{"review", "GET", "/v1/platform/failover-cutovers/" + id, nil, func(c *Client) error {
			out, e := c.PlatformGetFailover(ctx, id)
			if e == nil && (out.CutoverID != id || out.ReviewDigest != digest) {
				t.Fatal("lost review identity")
			}
			return e
		}},
		{"apply", "POST", "/v1/platform/failover-cutovers/" + id + "/apply", confirm, func(c *Client) error {
			out, e := c.PlatformApplyFailover(ctx, id, confirm)
			if e == nil && !out.Replayed {
				t.Fatal("lost replay receipt")
			}
			return e
		}},
		{"retry", "POST", "/v1/platform/failover-cutovers/" + id + "/retry-activation", confirm, func(c *Client) error { _, e := c.PlatformRetryFailoverActivation(ctx, id, confirm); return e }},
		{"prepare failback", "POST", "/v1/platform/failover-cutovers/" + id + "/prepare-failback", struct{}{}, func(c *Client) error { _, e := c.PlatformPrepareFailback(ctx, id); return e }},
		{"failback", "GET", "/v1/platform/failover-failbacks/" + failback, nil, func(c *Client) error {
			out, e := c.PlatformGetFailback(ctx, failback)
			if e == nil && (out.FailbackID != failback || out.ReviewDigest != digest) {
				t.Fatal("lost failback identity")
			}
			return e
		}},
		{"apply failback", "POST", "/v1/platform/failover-failbacks/" + failback + "/apply", confirm, func(c *Client) error {
			out, e := c.PlatformApplyFailback(ctx, failback, confirm)
			if e == nil && !out.Replayed {
				t.Fatal("lost failback replay receipt")
			}
			return e
		}},
		{"drill policy", "POST", base + "/failover-standby/drill-policy", map[string]int{"interval_minutes": 60}, func(c *Client) error {
			_, e := c.PlatformSetFailoverDrillPolicy(ctx, dep, &interval)
			return e
		}},
		{"drill policy off", "POST", base + "/failover-standby/drill-policy", map[string]any{"interval_minutes": nil}, func(c *Client) error {
			_, e := c.PlatformSetFailoverDrillPolicy(ctx, dep, nil)
			return e
		}},
		{"run drill", "POST", base + "/failover-standby/drills", struct{}{}, func(c *Client) error { _, e := c.PlatformRunFailoverDrill(ctx, dep); return e }},
		{"drill", "GET", "/v1/platform/failover-drills/" + drill, nil, func(c *Client) error {
			out, e := c.PlatformGetFailoverDrill(ctx, drill)
			if e == nil && out.DrillID != drill {
				t.Fatal("lost drill identity")
			}
			return e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != "" {
					t.Errorf("wrong route %s %s", r.Method, r.URL)
				}
				if tc.body != nil {
					var got any
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Fatal(err)
					}
					want, _ := json.Marshal(tc.body)
					actual, _ := json.Marshal(got)
					if string(want) != string(actual) {
						var normalized any
						_ = json.Unmarshal(want, &normalized)
						want, _ = json.Marshal(normalized)
						if string(want) != string(actual) {
							t.Errorf("body %s != %s", actual, want)
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.method == "POST" {
					w.WriteHeader(202)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"cutover_id": id, "failback_id": failback, "drill_id": drill, "review_digest": digest, "replayed": true, "receipt": map[string]any{"status": "fencing"}}})
			}))
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls %d", calls)
			}
		})
	}
}
func TestFailoverRefusesInvalidArgumentsBeforeHTTP(t *testing.T) {
	ctx := context.Background()
	dep := "dpl_" + strings.Repeat("a", 16)
	target := "dpl_" + strings.Repeat("b", 16)
	id := "fov_" + strings.Repeat("c", 24)
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid input must not send HTTP") }))
	assertError := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	for _, bad := range []string{"", dep + "\n", "../foreign", dep + "/extra"} {
		_, e := c.PlatformPairColdStandby(ctx, bad, target)
		assertError(e)
		_, e = c.PlatformPairColdStandby(ctx, dep, bad)
		assertError(e)
		_, e = c.PlatformGetFailoverStandby(ctx, bad)
		assertError(e)
		_, e = c.PlatformPrepareFailover(ctx, bad)
		assertError(e)
	}
	_, e := c.PlatformPairColdStandby(ctx, dep, dep)
	assertError(e)
	failback := "fbk_" + strings.Repeat("e", 24)
	for _, bad := range []string{"", id + "\n", "../foreign"} {
		_, e = c.PlatformGetFailover(ctx, bad)
		assertError(e)
		_, e = c.PlatformApplyFailover(ctx, bad, FailoverConfirmation{strings.Repeat("d", 64), true})
		assertError(e)
		_, e = c.PlatformPrepareFailback(ctx, bad)
		assertError(e)
	}
	// A cutover id is not a failback id, and the reverse.
	for _, bad := range []string{"", failback + "\n", "../foreign", id} {
		_, e = c.PlatformGetFailback(ctx, bad)
		assertError(e)
		_, e = c.PlatformApplyFailback(ctx, bad, FailoverConfirmation{strings.Repeat("d", 64), true})
		assertError(e)
	}
	_, e = c.PlatformPrepareFailback(ctx, failback)
	assertError(e)
	for _, bad := range []int{59, 10081, 0, -60} {
		_, e = c.PlatformSetFailoverDrillPolicy(ctx, dep, &bad)
		assertError(e)
	}
	for _, bad := range []string{"", dep + "\n", "../foreign"} {
		_, e = c.PlatformSetFailoverDrillPolicy(ctx, bad, nil)
		assertError(e)
		_, e = c.PlatformRunFailoverDrill(ctx, bad)
		assertError(e)
	}
	for _, bad := range []string{"", "fdr_" + strings.Repeat("a", 24) + "\n", id, failback, "../foreign"} {
		_, e = c.PlatformGetFailoverDrill(ctx, bad)
		assertError(e)
	}
	for _, confirm := range []FailoverConfirmation{{strings.Repeat("d", 64), false}, {"", true}, {strings.Repeat("d", 64) + "\n", true}} {
		_, e = c.PlatformApplyFailover(ctx, id, confirm)
		assertError(e)
		_, e = c.PlatformRetryFailoverActivation(ctx, id, confirm)
		assertError(e)
		_, e = c.PlatformApplyFailback(ctx, failback, confirm)
		assertError(e)
	}
	valid := FailoverSync{"bkp_" + strings.Repeat("e", 16), "bkp_" + strings.Repeat("f", 16), "cmd_" + strings.Repeat("a", 16)}
	for i := 0; i < 3; i++ {
		bad := valid
		switch i {
		case 0:
			bad.BackupID += "\n"
		case 1:
			bad.RestoreID = "../foreign"
		case 2:
			bad.DeployCommandID = ""
		}
		_, e = c.PlatformConfirmFailoverSync(ctx, dep, bad)
		assertError(e)
	}
}
