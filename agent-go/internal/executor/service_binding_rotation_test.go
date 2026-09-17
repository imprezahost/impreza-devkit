package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func rotationFixture() (string, sdkclient.ServiceBindingCredential, sdkclient.ServiceBindingRotationIntent) {
	consumer, c := generationFixture()
	candidate := c.ServiceBindingRef
	candidate.Revision = strings.Repeat("2", 64)
	intent := sdkclient.ServiceBindingRotationIntent{RotationID: "brot_" + strings.Repeat("3", 24), Mode: "rotate", Previous: c.ServiceBindingRef, Candidate: candidate}
	return consumer, c, intent
}

func rotationPayload(consumer string, intent sdkclient.ServiceBindingRotationIntent) sdkclient.DeployPayload {
	serving := intent.Candidate
	if intent.Mode == "abandon" {
		serving = intent.Previous
	}
	return sdkclient.DeployPayload{DeploymentID: consumer, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{
		Type:                           "docker-compose",
		Startup:                        &sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: 60},
		ServiceBindingProtocol:         sdkclient.ServiceBindingGenerationProtocol,
		ServiceBindings:                []sdkclient.ServiceBindingRef{serving},
		ServiceBindingRotationProtocol: sdkclient.ServiceBindingRotationProtocol,
		ServiceBindingRotation:         &intent,
	}}}
}

func TestServiceBindingRotationManifestBoundaries(t *testing.T) {
	consumer, _, intent := rotationFixture()
	for _, mode := range []string{"rotate", "abandon"} {
		intent.Mode = mode
		if err := validateServiceBindingManifest(rotationPayload(consumer, intent)); err != nil {
			t.Fatalf("reviewed %s rotation refused: %v", mode, err)
		}
	}
	intent.Mode = "rotate"
	for name, change := range map[string]func(*sdkclient.DeployPayload){
		"missing health policy": func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Startup = nil },
		"optional health":       func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Startup = &sdkclient.ManifestStartup{} },
		"invalid timeout":       func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Startup.TimeoutSeconds = 601 },
		"rotation protocol": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRotationProtocol = sdkclient.ServiceBindingGenerationRetirementProtocol
		},
		"missing intent": func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.ServiceBindingRotation = nil },
		"binding protocol": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingProtocol = sdkclient.ServiceBindingProtocol
		},
		"rotation identity": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRotation.RotationID = "bnd_" + strings.Repeat("3", 24)
		},
		"mode": func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.ServiceBindingRotation.Mode = "swap" },
		"candidate identity": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRotation.Candidate.BindingID = "bnd_" + strings.Repeat("9", 24)
		},
		"shared prefix": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRotation.Candidate.Revision = intent.Previous.Revision[:24] + strings.Repeat("9", 40)
		},
		"serving reference": func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.ServiceBindings[0] = intent.Previous },
		"abandon serving": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRotation.Mode = "abandon"
		},
		"retirement mixed": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRetirementProtocol = sdkclient.ServiceBindingGenerationRetirementProtocol
			p.Manifest.Runtime.ServiceBindingRetirements = []sdkclient.ServiceBindingRef{intent.Previous}
		},
		"build":   func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Build = &sdkclient.BuildContext{} },
		"runtime": func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Type = "docker" },
		"variable": func(p *sdkclient.DeployPayload) {
			p.Vars = map[string]any{"DATABASE_URL": "customer"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := rotationPayload(consumer, intent)
			change(&p)
			if err := validateServiceBindingManifest(p); err == nil {
				t.Fatal("unreviewed rotation accepted")
			}
		})
	}
	// A resolved payload legitimately carries the injected serving credential;
	// only the queued manifest refuses a runtime variable.
	resolved := rotationPayload(consumer, intent)
	resolved.Vars = map[string]any{"DATABASE_URL": "postgresql://resolved"}
	if err := validateServiceBindingManifest(resolved); err == nil {
		t.Fatal("queued rotation overwrote a runtime variable")
	}
	if _, _, err := validateRotationShape(consumer, resolved.Manifest.Runtime); err != nil {
		t.Fatal("resolved rotation shape refused")
	}
}

func TestServiceBindingRotationAuthorizationConfinement(t *testing.T) {
	consumer, c, intent := rotationFixture()
	_, _, previousLogin := bindingGenerationNames(intent.Previous)
	_, _, candidateLogin := bindingGenerationNames(intent.Candidate)
	retirement := func(ref sdkclient.ServiceBindingRef, login string) sdkclient.ServiceBindingRetirement {
		return sdkclient.ServiceBindingRetirement{ServiceBindingRef: ref, Username: login, Database: c.Database, AdminUser: c.AdminUser}
	}
	for _, mode := range []string{"rotate", "abandon"} {
		for _, fault := range []string{"valid", "wrong-protocol", "wrong-reference", "serving-target", "count", "privileged-target"} {
			t.Run(mode+"/"+fault, func(t *testing.T) {
				intent.Mode = mode
				target := intent.Previous
				login := previousLogin
				if mode == "abandon" {
					target, login = intent.Candidate, candidateLogin
				}
				response := sdkclient.ServiceBindingRetirements{Protocol: sdkclient.ServiceBindingRotationProtocol, Retirements: []sdkclient.ServiceBindingRetirement{retirement(target, login)}}
				switch fault {
				case "wrong-protocol":
					response.Protocol = sdkclient.ServiceBindingGenerationRetirementProtocol
				case "wrong-reference":
					response.Retirements[0].Revision = strings.Repeat("9", 64)
				case "serving-target":
					serving := intent.Candidate
					servingLogin := candidateLogin
					if mode == "abandon" {
						serving, servingLogin = intent.Previous, previousLogin
					}
					response.Retirements[0] = retirement(serving, servingLogin)
				case "count":
					response.Retirements = nil
				case "privileged-target":
					response.Retirements[0].Username = "postgres"
				}
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if r.Method != "POST" || r.URL.Path != "/v1/agent/service-binding-retirements/"+consumer || r.Header.Get("X-Agent-Secret") != "fixture" {
						t.Error("wrong retirement authorization transport")
					}
					var input map[string]string
					_ = json.NewDecoder(r.Body).Decode(&input)
					if input["command_id"] != "cmd_fixture" || input["control_token"] != "control" {
						t.Error("operation identity not forwarded")
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": response})
				}))
				defer server.Close()
				client, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
				if err != nil {
					t.Fatal(err)
				}
				p := rotationPayload(consumer, intent)
				cmd := &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandDeploy, ControlToken: "control"}
				err = (&Docker{Client: client}).prepareServiceBindingRotation(context.Background(), cmd, &p)
				if (err == nil) != (fault == "valid") || requests != 1 {
					t.Fatal("unverified rotation retirement authorization accepted")
				}
				if fault == "valid" {
					if len(p.ServiceBindingRetirementAuthorizations) != 1 || p.ServiceBindingRetirementAuthorizations[0].ServiceBindingRef != target {
						t.Fatal("rotation target authorization lost")
					}
					if err = validateResolvedRetirements(replacementPayload(p)); err != nil {
						t.Fatal("worker lost verified rotation intent")
					}
				} else if len(p.ServiceBindingRetirementAuthorizations) != 0 {
					t.Fatal("unverified authorization retained")
				}
			})
		}
	}
}

func TestServiceBindingRotationDiscardsQueuedAuthorizations(t *testing.T) {
	consumer, c, intent := rotationFixture()
	// Provisioning must fail for operational reasons in any environment, so the
	// provider points at a deployment that never has a container.
	intent.Previous.ProviderDeploymentID = "dpl_" + strings.Repeat("9", 16)
	intent.Candidate.ProviderDeploymentID = "dpl_" + strings.Repeat("9", 16)
	_, _, candidateLogin := bindingGenerationNames(intent.Candidate)
	_, _, previousLogin := bindingGenerationNames(intent.Previous)
	serving := sdkclient.ServiceBindingCredential{ServiceBindingRef: intent.Candidate, Username: candidateLogin, Database: c.Database, Password: strings.Repeat("e", 64), AdminUser: c.AdminUser}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out any
		switch r.URL.Path {
		case "/v1/agent/service-bindings/" + consumer:
			out = sdkclient.ServiceBindingCredentials{Protocol: sdkclient.ServiceBindingGenerationProtocol, Bindings: []sdkclient.ServiceBindingCredential{serving}}
		case "/v1/agent/service-binding-retirements/" + consumer:
			out = sdkclient.ServiceBindingRetirements{Protocol: sdkclient.ServiceBindingRotationProtocol, Retirements: []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: intent.Previous, Username: previousLogin, Database: c.Database, AdminUser: c.AdminUser}}}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": out})
	}))
	defer server.Close()
	client, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	p := rotationPayload(consumer, intent)
	p.ServiceBindingRetirementAuthorizations = []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: intent.Candidate, Username: "postgres", Database: c.Database, AdminUser: c.AdminUser}}
	cmd := &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandDeploy, ControlToken: "control"}
	// The provider has no container in any environment; authorization must still
	// be resolved from the JIT response alone before provisioning is attempted.
	_, err = (&Docker{Client: client}).prepareServiceBindings(context.Background(), cmd, &p)
	if err == nil || strings.Contains(err.Error(), serving.Password) {
		t.Fatal("rotation preparation leaked or skipped provisioning")
	}
	if len(p.ServiceBindingRetirementAuthorizations) != 1 || p.ServiceBindingRetirementAuthorizations[0].Username != previousLogin {
		t.Fatal("queued payload granted rotation retirement authority")
	}
}

func TestServiceBindingRotationOutcome(t *testing.T) {
	consumer, c, intent := rotationFixture()
	// The provider points at a deployment that never has a container, so the
	// retirement fails for operational reasons in any environment, not just
	// where Docker happens to be absent.
	unreachable := "dpl_" + strings.Repeat("9", 16)
	resolved := func(mode string) sdkclient.DeployPayload {
		intent.Mode = mode
		intent.Previous.ProviderDeploymentID = unreachable
		intent.Candidate.ProviderDeploymentID = unreachable
		p := rotationPayload(consumer, intent)
		target := intent.Previous
		if mode == "abandon" {
			target = intent.Candidate
		}
		_, _, login := bindingGenerationNames(target)
		p.ServiceBindingRetirementAuthorizations = []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: target, Username: login, Database: c.Database, AdminUser: c.AdminUser}}
		return p
	}
	for _, mode := range []string{"rotate", "abandon"} {
		p := resolved(mode)
		pending := "cleanup_pending"
		if mode == "abandon" {
			pending = "abandon_pending"
		}
		// No verified provider container exists here; retirement must fail
		// closed and still report the durable pending outcome.
		out := (&Docker{StateDir: t.TempDir()}).finishServiceBindingRotation(context.Background(), p, true)
		if out == nil || out.Status != pending || !out.DataRetained || out.RotationID != intent.RotationID || out.BindingID != intent.Previous.BindingID || out.PreviousRevision != intent.Previous.Revision || out.CandidateRevision != intent.Candidate.Revision {
			t.Fatalf("pending %s outcome mismatch: %+v", mode, out)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]any
		if json.Unmarshal(raw, &keys) != nil || len(keys) != 6 {
			t.Fatal("rotation outcome keys changed")
		}
		// Unhealthy replacements and unverified payloads never retire.
		var nilDocker *Docker
		if out = nilDocker.finishServiceBindingRotation(context.Background(), p, false); out.Status != pending {
			t.Fatal("unhealthy replacement retired a login")
		}
		bad := resolved(mode)
		bad.ServiceBindingRetirementAuthorizations[0].Username = "postgres"
		if out = nilDocker.finishServiceBindingRotation(context.Background(), bad, true); out.Status != pending {
			t.Fatal("unverified rotation retired a login")
		}
		if retirements := nilDocker.finishServiceBindingRetirements(context.Background(), p); retirements != nil {
			t.Fatal("rotation reported a removal outcome")
		}
	}
	var plain *Docker
	p := resolved("rotate")
	p.Manifest.Runtime.ServiceBindingRotation = nil
	if out := plain.finishServiceBindingRotation(context.Background(), p, true); out != nil {
		t.Fatal("rotation outcome without intent")
	}
}

// The supervised worker receives the payload through replacementPayload and a
// JSON file, and returns its result through a JSON receipt. The rotation intent,
// its retirement authorization and the outcome must survive both boundaries.
func TestServiceBindingRotationWorkerPlumbing(t *testing.T) {
	consumer, c, intent := rotationFixture()
	p := rotationPayload(consumer, intent)
	_, _, previousLogin := bindingGenerationNames(intent.Previous)
	p.ServiceBindingRetirementAuthorizations = []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: intent.Previous, Username: previousLogin, Database: c.Database, AdminUser: c.AdminUser}}
	p.Vars = map[string]any{"DATABASE_URL": "postgresql://injected", "KEEP": "value"}
	raw, err := json.Marshal(replacementPayload(p))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "service_binding_rotation") {
		t.Fatal("rotation intent missing from worker payload")
	}
	var decoded sdkclient.DeployPayload
	if json.Unmarshal(raw, &decoded) != nil || decoded.Manifest.Runtime.ServiceBindingRotation == nil {
		t.Fatal("rotation intent lost before the worker")
	}
	if err := validateResolvedRetirements(decoded); err != nil {
		t.Fatalf("worker lost verified rotation intent: %v", err)
	}
	receipt := replacementReceipt{Version: 1, Result: sdkclient.DeployResult{CommandID: "cmd_fixture", Status: "success", ServiceBindingRotation: &sdkclient.ServiceBindingRotationResult{RotationID: intent.RotationID, Status: "completed", DataRetained: true}}}
	raw, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var decodedReceipt replacementReceipt
	if json.Unmarshal(raw, &decodedReceipt) != nil || decodedReceipt.Result.ServiceBindingRotation == nil || decodedReceipt.Result.ServiceBindingRotation.Status != "completed" || decodedReceipt.Result.ServiceBindingRotation.RotationID != intent.RotationID {
		t.Fatal("rotation outcome lost in the durable worker receipt")
	}
}

func TestServiceBindingRotationRedaction(t *testing.T) {
	consumer, c, intent := rotationFixture()
	_, _, previousLogin := bindingGenerationNames(intent.Previous)
	_, _, candidateLogin := bindingGenerationNames(intent.Candidate)
	url := func(login, password string) string {
		return "postgresql://" + login + ":" + password + "@pg_" + c.ProviderDeploymentID + ":5432/" + c.Database + "?sslmode=disable"
	}
	previousPassword, candidatePassword := strings.Repeat("4", 64), strings.Repeat("5", 64)
	previousURL, candidateURL := url(previousLogin, previousPassword), url(candidateLogin, candidatePassword)
	values := serviceBindingEnvRedactions([]byte("DATABASE_URL=" + candidateURL + "\n"))
	for key, value := range serviceBindingEnvRedactions([]byte("DATABASE_URL=" + previousURL + "\n")) {
		values[key] = value
	}
	if len(values) != 4 {
		t.Fatal("both rotation revisions must be redacted")
	}
	result := sdkclient.DeployResult{CommandID: "cmd_fixture", Status: "success", DeploymentID: consumer, LogsTail: "serving " + candidateURL + " retired " + previousPassword}
	redactServiceBindingResult(&result, values)
	raw, _ := json.Marshal(result)
	text := string(raw)
	for _, secret := range []string{candidateURL, previousURL, candidatePassword, previousPassword} {
		if strings.Contains(text, secret) {
			t.Fatal("rotation credential leaked into the deploy result")
		}
	}
	if !strings.Contains(text, "[redacted]") || result.Status != "success" {
		t.Fatal("rotation redaction corrupted the result")
	}
}
