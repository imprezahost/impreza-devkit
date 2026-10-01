package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// The quiesce block rides the deploy payload of a restore transport job,
// and the receipt comes back on the deploy result: the wire contract of
// restore-quiesce-v1.
func TestDeployQuiesceRoundTrip(t *testing.T) {
	raw := `{
		"deployment_id": "bkpjob_0123456789abcdef",
		"manifest": {"runtime": {"type": "docker-compose", "compose_yaml": "services: {job: {image: i}}"}},
		"vars": {"IMPREZA_TARGET": "dpl_aaaaaaaaaaaaaaaa"},
		"quiesce": {"target": "dpl_aaaaaaaaaaaaaaaa", "stop_timeout_seconds": 60}
	}`
	var payload DeployPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Quiesce == nil || payload.Quiesce.Target != "dpl_aaaaaaaaaaaaaaaa" || payload.Quiesce.StopTimeoutSeconds != 60 {
		t.Fatalf("quiesce block did not decode: %+v", payload.Quiesce)
	}
	encoded, err := json.Marshal(&payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"quiesce":{"target":"dpl_aaaaaaaaaaaaaaaa","stop_timeout_seconds":60}`) {
		t.Fatalf("quiesce block did not encode: %s", encoded)
	}
	// Absent block stays absent: jobs without quiesce keep the old wire shape.
	plain := DeployPayload{DeploymentID: "bkpjob_0123456789abcdef"}
	encoded, err = json.Marshal(&plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "quiesce") {
		t.Fatalf("quiesce leaked into a plain payload: %s", encoded)
	}
}

func TestRestoreQuiesceResultRoundTrip(t *testing.T) {
	raw := `{
		"command_id": "cmd_x",
		"status": "success",
		"deployment_id": "bkpjob_0123456789abcdef",
		"restore_quiesce": {"target": "dpl_aaaaaaaaaaaaaaaa", "stopped": true, "job_exit_code": 0, "undone": false}
	}`
	var result DeployResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.RestoreQuiesce == nil || !result.RestoreQuiesce.Stopped || result.RestoreQuiesce.Target != "dpl_aaaaaaaaaaaaaaaa" || result.RestoreQuiesce.JobExitCode != 0 {
		t.Fatalf("receipt did not decode: %+v", result.RestoreQuiesce)
	}
	if RestoreQuiesceProtocol != "restore-quiesce-v1" {
		t.Fatalf("protocol constant drifted: %s", RestoreQuiesceProtocol)
	}
}
