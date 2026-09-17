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

func TestServiceBindingGenerationProtocolConfinement(t *testing.T) {
	consumer, credential := generationFixture()
	for _, operation := range []string{"credential", "retirement"} {
		for _, fault := range []string{"downgrade", "revision", "legacy_login"} {
			t.Run(operation+"/"+fault, func(t *testing.T) {
				c := credential
				protocol := sdkclient.ServiceBindingGenerationProtocol
				if operation == "retirement" {
					protocol = sdkclient.ServiceBindingGenerationRetirementProtocol
				}
				responseProtocol := protocol
				switch fault {
				case "downgrade":
					if operation == "credential" {
						responseProtocol = sdkclient.ServiceBindingProtocol
					} else {
						responseProtocol = sdkclient.ServiceBindingRetirementProtocol
					}
				case "revision":
					c.Revision = strings.Repeat("8", 64)
				case "legacy_login":
					c.Username = c.Database
				}
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if r.Header.Get("X-Agent-Id") != "agt_fixture" {
						t.Error("missing agent identity")
					}
					var payload map[string]string
					if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["command_id"] != "cmd_"+strings.Repeat("a", 16) || payload["control_token"] != strings.Repeat("b", 32) {
						t.Error("missing command authority")
					}
					var response any = sdkclient.ServiceBindingCredentials{Protocol: responseProtocol, Bindings: []sdkclient.ServiceBindingCredential{c}}
					if operation == "retirement" {
						response = sdkclient.ServiceBindingRetirements{Protocol: responseProtocol, Retirements: []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}}}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": response})
				}))
				defer server.Close()
				client, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
				if err != nil {
					t.Fatal(err)
				}
				d := &Docker{Client: client}
				p := sdkclient.DeployPayload{DeploymentID: consumer, Vars: map[string]any{"KEEP": "value"}, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose"}}}
				if operation == "credential" {
					p.Manifest.Runtime.ServiceBindingProtocol = protocol
					p.Manifest.Runtime.ServiceBindings = []sdkclient.ServiceBindingRef{credential.ServiceBindingRef}
				} else {
					p.Manifest.Runtime.ServiceBindingRetirementProtocol = protocol
					p.Manifest.Runtime.ServiceBindingRetirements = []sdkclient.ServiceBindingRef{credential.ServiceBindingRef}
				}
				cmd := &sdkclient.PollCommand{ID: "cmd_" + strings.Repeat("a", 16), Kind: sdkclient.CommandDeploy, ControlToken: strings.Repeat("b", 32)}
				values, err := d.prepareServiceBindings(context.Background(), cmd, &p)
				if err == nil || len(values) != 0 || len(p.Vars) != 1 || len(p.ServiceBindingRetirementAuthorizations) != 0 || requests != 1 || strings.Contains(err.Error(), credential.Password) {
					t.Fatal("invalid generation response reached runtime or leaked a credential")
				}
			})
		}
	}
}

func TestServiceBindingGenerationRuntimePreservation(t *testing.T) {
	consumer, c := generationFixture()
	alias := "impreza-binding-" + strings.TrimPrefix(c.BindingID, "bnd_")
	value := "postgresql://" + c.Username + ":" + c.Password + "@pg_" + c.ProviderDeploymentID + ":5432/" + c.Database + "?sslmode=disable"
	model := func(url string) []byte {
		raw, err := json.Marshal(map[string]any{"networks": map[string]any{alias: map[string]any{"external": true, "name": c.ProviderDeploymentID + "_default"}}, "services": map[string]any{"app": map[string]any{"networks": map[string]any{alias: nil}, "environment": map[string]any{"DATABASE_URL": url}}}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	vars, err := preservedServiceBindingVars(consumer, model(value), map[string]any{"DOMAIN_URL": "https://next.example.test"})
	if err != nil || vars["DATABASE_URL"] != value {
		t.Fatal("route update lost managed generation credential")
	}
	for _, login := range []string{"ibg_" + strings.Repeat("0", 24) + "_" + strings.Repeat("1", 24), c.Username + "x", c.Username[:len(c.Username)-1]} {
		if _, err := preservedServiceBindingVars(consumer, model(strings.Replace(value, c.Username, login, 1)), nil); err == nil {
			t.Fatal("foreign or malformed generation login was preserved")
		}
	}
	if _, err := preservedServiceBindingVars(consumer, model(value), map[string]any{"DATABASE_URL": "override"}); err == nil {
		t.Fatal("managed generation credential could be overwritten")
	}
	redactions := serviceBindingEnvRedactions([]byte("DATABASE_URL=" + value + "\nKEEP=value\n"))
	clean := redactServiceBindingText(value+" "+c.Password, redactions)
	if len(redactions) != 2 || strings.Contains(clean, c.Password) || strings.Contains(clean, value) {
		t.Fatal("generation URL/password leaked through runtime log redaction")
	}
	if serviceBindingRollbackCompatible(model(value), model(value)) != nil {
		t.Fatal("same generation rollback refused")
	}
	next := strings.Replace(value, c.Username, strings.Replace(c.Username, strings.Repeat("1", 24), strings.Repeat("2", 24), 1), 1)
	if serviceBindingRollbackCompatible(model(value), model(next)) == nil {
		t.Fatal("rollback changed generation")
	}
	legacy := strings.Replace(value, c.Username, c.Database, 1)
	if serviceBindingRollbackCompatible(model(value), model(legacy)) == nil {
		t.Fatal("rollback silently changed binding protocol")
	}
}

func TestServiceBindingGenerationRetirementRequiresVerifiedProtocol(t *testing.T) {
	consumer, c := generationFixture()
	p := sdkclient.DeployPayload{DeploymentID: consumer, ServiceBindingRetirementAuthorizations: []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}}, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ServiceBindingRetirementProtocol: "unknown", ServiceBindingRetirements: []sdkclient.ServiceBindingRef{c.ServiceBindingRef}}}}
	// A nil executor proves the invalid protocol never reaches Docker.
	var d *Docker
	out := d.finishServiceBindingRetirements(context.Background(), p)
	if len(out) != 1 || out[0].Status != "pending" {
		t.Fatal("unverified generation retirement reported completion")
	}
	p.Manifest.Runtime.ServiceBindingRetirementProtocol = sdkclient.ServiceBindingGenerationRetirementProtocol
	if validateResolvedRetirements(p) != nil {
		t.Fatal("verified generation retirement shape refused")
	}
	p.Manifest.Runtime.ServiceBindingProtocol = sdkclient.ServiceBindingGenerationProtocol
	p.Manifest.Runtime.ServiceBindings = []sdkclient.ServiceBindingRef{c.ServiceBindingRef}
	if validateServiceBindingManifest(p) == nil {
		t.Fatal("creation and retirement protocols were mixed")
	}
}
