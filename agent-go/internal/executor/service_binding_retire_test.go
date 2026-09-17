package executor

import (
	"context"
	"encoding/json"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceBindingRetirementRequiresFreshAuthorization(t *testing.T) {
	consumer, c := bindingFixture()
	authorization := sdkclient.ServiceBindingRetirement{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}
	base := sdkclient.DeployPayload{DeploymentID: consumer, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ServiceBindingRetirementProtocol: sdkclient.ServiceBindingRetirementProtocol, ServiceBindingRetirements: []sdkclient.ServiceBindingRef{c.ServiceBindingRef}}}}
	injected := base
	injected.ServiceBindingRetirementAuthorizations = []sdkclient.ServiceBindingRetirement{authorization}
	if _, err := (&Docker{}).prepareServiceBindings(context.Background(), &sdkclient.PollCommand{Kind: sdkclient.CommandDeploy}, &injected); err == nil || len(injected.ServiceBindingRetirementAuthorizations) != 0 {
		t.Fatal("queued payload granted retirement authority")
	}
	for _, mode := range []string{"valid", "wrong-reference", "wrong-protocol", "privileged-target", "missing-control"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/v1/agent/service-binding-retirements/"+consumer || r.Header.Get("X-Agent-Secret") != "fixture" {
					t.Error("wrong retirement authorization transport")
				}
				var input map[string]string
				_ = json.NewDecoder(r.Body).Decode(&input)
				if input["command_id"] != "cmd_fixture" || input["control_token"] != "control" {
					t.Error("operation identity not forwarded")
				}
				out := sdkclient.ServiceBindingRetirements{Protocol: sdkclient.ServiceBindingRetirementProtocol, Retirements: []sdkclient.ServiceBindingRetirement{authorization}}
				switch mode {
				case "wrong-reference":
					out.Retirements[0].Revision = strings.Repeat("9", 64)
				case "wrong-protocol":
					out.Protocol = "unknown"
				case "privileged-target":
					out.Retirements[0].Username = "postgres"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": out})
			}))
			defer server.Close()
			client, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			p := base
			cmd := &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandDeploy, ControlToken: "control"}
			if mode == "missing-control" {
				cmd.ControlToken = ""
			}
			_, err = (&Docker{Client: client}).prepareServiceBindings(context.Background(), cmd, &p)
			if (err == nil) != (mode == "valid") || (mode == "missing-control" && calls != 0) {
				t.Fatal("unverified retirement authorization accepted")
			}
			if mode == "valid" {
				if err = validateResolvedRetirements(replacementPayload(p)); err != nil {
					t.Fatal("worker lost verified retirement intent")
				}
			}
		})
	}
}

func TestServiceBindingRetireIdentity(t *testing.T) {
	consumer, c := bindingFixture()
	r := sdkclient.ServiceBindingRetirement{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}
	sql, err := postgresBindingRetireSQL(consumer, r)
	if err != nil || strings.Contains(sql, c.Password) || strings.Contains(sql, "DROP DATABASE") || !strings.Contains(sql, "NOLOGIN PASSWORD NULL") {
		t.Fatal("retirement must disable only a verified login and retain data")
	}
	for _, change := range []func(*sdkclient.ServiceBindingRetirement){
		func(v *sdkclient.ServiceBindingRetirement) { v.Database = "postgres" },
		func(v *sdkclient.ServiceBindingRetirement) { v.Username = "postgres" },
		func(v *sdkclient.ServiceBindingRetirement) { v.AdminUser = "--command=bad" },
		func(v *sdkclient.ServiceBindingRetirement) { v.BindingID = "bnd_';DROP ROLE postgres;--" },
		func(v *sdkclient.ServiceBindingRetirement) { v.ProviderDeploymentID = consumer },
	} {
		bad := r
		change(&bad)
		if _, err := postgresBindingRetireSQL(consumer, bad); err == nil {
			t.Fatal("unverified retirement identity accepted")
		}
	}
}
