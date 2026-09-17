package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestServiceBindingCredentialResponseConfinement(t *testing.T) {
	consumer, base := bindingFixture()
	for _, name := range []string{"revision", "provider", "binding", "variable", "count", "protocol", "upstream_error"} {
		t.Run(name, func(t *testing.T) {
			credential := base
			response := sdkclient.ServiceBindingCredentials{Protocol: sdkclient.ServiceBindingProtocol}
			switch name {
			case "revision":
				credential.Revision = strings.Repeat("9", 64)
			case "provider":
				credential.ProviderDeploymentID = "dpl_" + strings.Repeat("9", 16)
			case "binding":
				credential.BindingID = "bnd_" + strings.Repeat("9", 24)
			case "variable":
				credential.Variable = "PATH"
			case "protocol":
				response.Protocol = "unexpected"
			}
			response.Bindings = []sdkclient.ServiceBindingCredential{credential}
			if name == "count" {
				response.Bindings = nil
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/v1/agent/service-bindings/"+consumer || r.Header.Get("X-Agent-Id") != "agt_fixture" {
					t.Error("incorrect agent credential request")
				}
				if name == "upstream_error" {
					w.WriteHeader(403)
					_, _ = w.Write([]byte(base.Password))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": response})
			}))
			defer server.Close()
			client, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			d := &Docker{Client: client}
			p := sdkclient.DeployPayload{DeploymentID: consumer, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ServiceBindingProtocol: sdkclient.ServiceBindingProtocol, ServiceBindings: []sdkclient.ServiceBindingRef{base.ServiceBindingRef}}}, Vars: map[string]any{"KEEP": "value"}}
			cmd := &sdkclient.PollCommand{ID: "cmd_" + strings.Repeat("a", 16), Kind: sdkclient.CommandDeploy, ControlToken: strings.Repeat("b", 32)}
			values, err := d.prepareServiceBindings(context.Background(), cmd, &p)
			if err == nil || len(values) != 0 || len(p.Vars) != 1 || requests != 1 || strings.Contains(err.Error(), base.Password) {
				t.Fatal("mismatched response reached runtime or exposed credential")
			}
		})
	}
}

func TestServiceBindingManifestAndResultBoundaries(t *testing.T) {
	consumer, c := bindingFixture()
	p := sdkclient.DeployPayload{DeploymentID: consumer, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ServiceBindingProtocol: sdkclient.ServiceBindingProtocol, ServiceBindings: []sdkclient.ServiceBindingRef{c.ServiceBindingRef}}}}
	if err := validateServiceBindingManifest(p); err != nil {
		t.Fatal(err)
	}
	p.Vars = map[string]any{"DATABASE_URL": "existing"}
	if validateServiceBindingManifest(p) == nil {
		t.Fatal("existing variable overwritten")
	}
	p.Vars = nil
	p.Manifest.Runtime.ServiceBindingProtocol = ""
	if validateServiceBindingManifest(p) == nil {
		t.Fatal("unversioned binding accepted")
	}
	result := sdkclient.DeployResult{Status: "failed", Error: "failed " + c.Password, LogsTail: c.Password, AdminCredentials: map[string]string{"unexpected": c.Password}}
	redactServiceBindingResult(&result, map[string]string{"password": c.Password})
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), c.Password) || result.Status != "failed" {
		t.Fatal("result redaction failed")
	}
}

func TestServiceBindingRoutePreservation(t *testing.T) {
	consumer, c := bindingFixture()
	alias := "impreza-binding-" + strings.TrimPrefix(c.BindingID, "bnd_")
	value := "postgresql://" + c.Username + ":" + c.Password + "@pg_" + c.ProviderDeploymentID + ":5432/" + c.Database + "?sslmode=disable"
	for _, change := range []string{"none", "collision", "password", "provider", "external", "membership", "second_service", "missing_url", "second_binding"} {
		t.Run(change, func(t *testing.T) {
			vars := map[string]any{"DOMAIN_URL": "https://next.example.test"}
			network := map[string]any{"name": c.ProviderDeploymentID + "_default", "external": true}
			networks := map[string]any{alias: network}
			env := map[string]any{"DATABASE_URL": value}
			appNetworks := map[string]any{alias: nil}
			services := map[string]any{"app": map[string]any{"environment": env, "networks": appNetworks}}
			switch change {
			case "collision":
				vars["DATABASE_URL"] = "attacker"
			case "password":
				env["DATABASE_URL"] = strings.Replace(value, c.Password, "bad", 1)
			case "provider":
				network["name"] = "foreign_default"
			case "external":
				network["external"] = false
			case "membership":
				delete(appNetworks, alias)
			case "second_service":
				services["other"] = services["app"]
			case "missing_url":
				delete(env, "DATABASE_URL")
			case "second_binding":
				networks["impreza-binding-"+strings.Repeat("9", 24)] = network
			}
			raw, _ := json.Marshal(map[string]any{"networks": networks, "services": services})
			got, err := preservedServiceBindingVars(consumer, raw, vars)
			if change == "none" {
				if err != nil || got["DATABASE_URL"] != value || got["DOMAIN_URL"] != vars["DOMAIN_URL"] {
					t.Fatal("binding was not preserved")
				}
				if _, ok := vars["DATABASE_URL"]; ok {
					t.Fatal("caller variables mutated")
				}
			} else if err == nil || strings.Contains(err.Error(), c.Password) {
				t.Fatal("unverified binding accepted or leaked")
			}
		})
	}
	vars := map[string]any{"DATABASE_URL": "customer-managed"}
	got, err := preservedServiceBindingVars(consumer, []byte(`{"networks":{"default":{"name":"ordinary"}}}`), vars)
	if err != nil || got["DATABASE_URL"] != vars["DATABASE_URL"] {
		t.Fatal("unbound customer variable changed")
	}
}

func TestServiceBindingRedactsBeforeChunking(t *testing.T) {
	_, c := bindingFixture()
	value := "postgresql://" + c.Username + ":" + c.Password + "@pg_" + c.ProviderDeploymentID + ":5432/" + c.Database + "?sslmode=disable"
	values := serviceBindingEnvRedactions([]byte("DATABASE_URL=" + value + "\n"))
	if len(values) != 2 {
		t.Fatal("generated runtime secrets not identified")
	}
	text := strings.Repeat("x", logsChunkSize-5) + c.Password + " marker " + value
	clean := redactServiceBindingText(text, values)
	if strings.Contains(clean, c.Password) || !strings.Contains(clean, "marker") || !strings.Contains(clean, "[redacted]") {
		t.Fatal("chunk boundary exposes a generated credential")
	}
}

func TestServiceBindingStructuredLogs(t *testing.T) {
	_, c := bindingFixture()
	var output bytes.Buffer
	base := &Docker{Log: slog.New(slog.NewJSONHandler(&output, nil))}
	secured := base.withServiceBindingLogRedaction(map[string]string{"password": c.Password})
	secured.Log.With("saved", c.Password).WithGroup("nested").Error("failure "+c.Password, "err", errors.New("provider "+c.Password), "group", slog.GroupValue(slog.String("detail", c.Password)))
	if strings.Contains(output.String(), c.Password) || !strings.Contains(output.String(), "[redacted]") {
		t.Fatal("structured log exposed binding secret")
	}
	if secured == base || secured.Log == base.Log {
		t.Fatal("operation mutated shared logger")
	}
}
