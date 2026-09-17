package client

import (
	"encoding/json"
	"strings"
	"testing"
)

func rotationFixture() (ServiceBindingRef, ServiceBindingRef) {
	previous := ServiceBindingRef{BindingID: "bnd_" + strings.Repeat("a", 24), ProviderDeploymentID: "dpl_" + strings.Repeat("b", 16), Variable: "DATABASE_URL", Revision: strings.Repeat("1", 64)}
	candidate := previous
	candidate.Revision = strings.Repeat("2", 64)
	return previous, candidate
}

func TestServiceBindingRotationManifestRoundTrip(t *testing.T) {
	previous, candidate := rotationFixture()
	runtime := ManifestRuntime{
		Type:                         "docker-compose",
		ServiceBindingProtocol:       ServiceBindingGenerationProtocol,
		ServiceBindings:              []ServiceBindingRef{candidate},
		ServiceBindingRotationProtocol: ServiceBindingRotationProtocol,
		ServiceBindingRotation:       &ServiceBindingRotationIntent{RotationID: "brot_" + strings.Repeat("c", 24), Mode: "rotate", Previous: previous, Candidate: candidate},
	}
	raw, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ManifestRuntime
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ServiceBindingRotation == nil || *decoded.ServiceBindingRotation != *runtime.ServiceBindingRotation || decoded.ServiceBindingRotationProtocol != ServiceBindingRotationProtocol || len(decoded.ServiceBindings) != 1 || decoded.ServiceBindings[0] != candidate {
		t.Fatal("rotation intent lost in transport")
	}
	plain, err := json.Marshal(ManifestRuntime{Type: "docker-compose"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "service_binding_rotation") {
		t.Fatal("absent rotation serialized")
	}
}

func TestServiceBindingRotationResultContract(t *testing.T) {
	previous, candidate := rotationFixture()
	result := DeployResult{CommandID: "cmd_fixture", Status: "success", ServiceBindingRotation: &ServiceBindingRotationResult{
		BindingID: previous.BindingID, CandidateRevision: candidate.Revision, DataRetained: true, PreviousRevision: previous.Revision, RotationID: "brot_" + strings.Repeat("c", 24), Status: "completed",
	}}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	outcome, ok := body["service_binding_rotation"].(map[string]any)
	if !ok {
		t.Fatal("rotation outcome missing")
	}
	want := map[string]bool{"binding_id": true, "candidate_revision": true, "data_retained": true, "previous_revision": true, "rotation_id": true, "status": true}
	if len(outcome) != len(want) {
		t.Fatalf("outcome keys changed: %v", outcome)
	}
	for key := range outcome {
		if !want[key] {
			t.Fatalf("unexpected outcome key %q", key)
		}
	}
	if outcome["data_retained"] != true || outcome["status"] != "completed" || outcome["rotation_id"] != "brot_"+strings.Repeat("c", 24) {
		t.Fatal("outcome values mismatch")
	}
	var decoded DeployResult
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.ServiceBindingRotation == nil || *decoded.ServiceBindingRotation != *result.ServiceBindingRotation {
		t.Fatal("rotation outcome lost in transport")
	}
	failed, err := json.Marshal(DeployResult{CommandID: "cmd_fixture", Status: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(failed), "service_binding_rotation") {
		t.Fatal("failed result carried a rotation outcome")
	}
}
