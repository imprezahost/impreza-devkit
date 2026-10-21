package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

// Uses only the parent's real-caller harness and JSON wire API, so this exact
// test can run on the pre-readiness base without copying candidate production.
func TestDeploymentReadinessSecondCallerWire(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		input                          deploymentProbeCase
		installation, endpoint, reason string
	}{
		{"unverified", deploymentProbeCase{routes: deploymentProbeClearnet()}, "COMPLETED", "UNVERIFIED", "PROBE_NOT_CONFIRMED"},
		{"confirmed", deploymentProbeCase{routes: deploymentProbeClearnet(), probeResult: true}, "COMPLETED", "CONFIRMED", "PROBE_PASSED"},
		{"routefailure", deploymentProbeCase{routes: deploymentProbeClearnet(), failDocker: "exec", probeResult: true}, "COMPLETED", "REFUSED", "ROUTE_APPLY_FAILED"},
		{"failedinstall", deploymentProbeCase{routes: deploymentProbeClearnet(), failDocker: "compose", probeResult: true}, "NOT_COMPLETED", "NOT_CHECKED", "NO_PROBE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deploymentProbeRun(t, tc.input)
			deploymentProbeRequireCaller(t, got)
			raw, err := json.Marshal(got.result)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err = json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			data, ok := wire["readiness"]
			if !ok {
				t.Fatal("missing finite readiness on actual caller wire")
			}
			var receipt map[string]string
			if err = json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			if len(receipt) != 6 || receipt["protocol"] != "F60-READINESS-1" || receipt["scope"] != "FIRST_CLEARNET_ROUTE" || receipt["diagnostics"] != "NOT_CHECKED" || receipt["installation"] != tc.installation || receipt["public_endpoint"] != tc.endpoint || receipt["reason"] != tc.reason {
				t.Fatalf("incorrect caller receipt: %s", data)
			}
			if tc.installation == "NOT_COMPLETED" {
				if got.result.Status == "success" || len(got.calls) != 0 {
					t.Fatal("failed install probed/promoted")
				}
			} else {
				if got.result.Status != "success" || len(got.calls) != 1 {
					t.Fatal("advisory policy or single-probe changed")
				}
				want := "public HTTPS not confirmed on "
				if tc.input.probeResult {
					want = "public HTTPS confirmed on "
				}
				if !strings.Contains(got.result.LogsTail, want+deploymentProbeHost) {
					t.Fatal("legacy probe note changed")
				}
			}
		})
	}
}
