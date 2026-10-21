package client

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeploymentReadinessLegacyAndHostileReadiness(t *testing.T) {
	for _, raw := range []string{
		`{ "status":"success" }`,
		`{ "status":"running", "readiness":{"protocol":"F60-READINESS-1","installation":"COMPLETED","public_endpoint":"SECRET_CANARY","reason":"SECRET_CANARY","scope":"FIRST_CLEARNET_ROUTE","diagnostics":"NOT_CHECKED"}}`,
		`{ "status":"success", "readiness":{"protocol":"F60-READINESS-1","installation":"NOT_COMPLETED","public_endpoint":"CONFIRMED","reason":"PROBE_PASSED","scope":"FIRST_CLEARNET_ROUTE","diagnostics":"NOT_CHECKED"}}`,
	} {
		var result DeployResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		safe := SafeDeploymentReadiness(result.Readiness)
		b, err := json.Marshal(safe)
		if err != nil {
			t.Fatal(err)
		}
		if safe.PublicEndpoint != "UNKNOWN" || strings.Contains(string(b), "SECRET_CANARY") {
			t.Fatal("unsafe projection")
		}
	}
}

func TestDeploymentReadinessReadinessClosedTuples(t *testing.T) {
	for _, pair := range [][2]string{{"CONFIRMED", "PROBE_PASSED"}, {"UNVERIFIED", "PROBE_NOT_CONFIRMED"}, {"UNVERIFIED", "CANCELLED"}, {"REFUSED", "ROUTE_APPLY_FAILED"}, {"NOT_APPLICABLE", "ONION_ONLY"}, {"NOT_CHECKED", "NO_PROBE"}} {
		r := DeploymentReadiness{"F60-READINESS-1", "COMPLETED", pair[0], pair[1], "FIRST_CLEARNET_ROUTE", "NOT_CHECKED"}
		if SafeDeploymentReadiness(&r) != r {
			t.Fatal("valid tuple refused")
		}
	}
}
