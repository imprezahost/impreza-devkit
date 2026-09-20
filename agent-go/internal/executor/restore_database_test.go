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

func restoreFixture() (sdkclient.DeployPayload, sdkclient.ServiceBindingCredential) {
	consumer, c := generationFixture()
	spec := sdkclient.RestoreDatabaseSpec{
		ServiceBindingRef:    c.ServiceBindingRef,
		Protocol:             sdkclient.ServiceBindingRestoreProtocol,
		ConsumerDeploymentID: consumer,
		RestoreDatabase:      "imp_restore_" + strings.Repeat("8", 16),
		ExpectedTables:       7,
		DumpSHA256:           strings.Repeat("a", 64),
	}
	p := sdkclient.DeployPayload{
		DeploymentID:  "rstjob_" + strings.Repeat("5", 16),
		RestorePlanID: "rspl_" + strings.Repeat("6", 24),
		Vars:          map[string]any{"IMPREZA_JOB": "bkp_" + strings.Repeat("3", 16)},
		Manifest:      sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", RestoreDatabase: &spec}},
	}
	return p, c
}

func TestRestoreDatabaseManifestBoundaries(t *testing.T) {
	base, c := restoreFixture()
	if err := validateRestoreDatabaseSpec(base); err != nil {
		t.Fatal(err)
	}
	plain := base
	plain.Manifest.Runtime.RestoreDatabase = nil
	if err := validateRestoreDatabaseSpec(plain); err != nil {
		t.Fatal("ordinary deploy refused")
	}
	for name, change := range map[string]func(*sdkclient.DeployPayload){
		"protocol": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.RestoreDatabase.Protocol = sdkclient.ServiceBindingBackupProtocol
		},
		"deployment": func(p *sdkclient.DeployPayload) { p.DeploymentID = "dpl_" + strings.Repeat("c", 16) },
		"build":      func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Build = &sdkclient.BuildContext{} },
		"runtime":    func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Type = "docker" },
		"database name": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.RestoreDatabase.RestoreDatabase = "imp_main"
		},
		"live database as target": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.RestoreDatabase.RestoreDatabase = "imp_" + strings.Repeat("a", 24)
		},
		"negative tables": func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.RestoreDatabase.ExpectedTables = -1 },
		"dump checksum":   func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.RestoreDatabase.DumpSHA256 = "short" },
		"consumer": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.RestoreDatabase.ConsumerDeploymentID = "rstjob_" + strings.Repeat("5", 16)
		},
		"self provider": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.RestoreDatabase.ProviderDeploymentID = p.Manifest.Runtime.RestoreDatabase.ConsumerDeploymentID
		},
		"binding mixed": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindings = []sdkclient.ServiceBindingRef{c.ServiceBindingRef}
			p.Manifest.Runtime.ServiceBindingProtocol = sdkclient.ServiceBindingGenerationProtocol
		},
		"backup mixed": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.BackupDatabase = &sdkclient.BackupDatabaseSpec{}
		},
		"rotation mixed": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRotationProtocol = sdkclient.ServiceBindingRotationProtocol
		},
		"variable":          func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.RestoreDatabase.Variable = "PGPASSWORD" },
		"variable occupied": func(p *sdkclient.DeployPayload) { p.Vars["DATABASE_URL"] = "untrusted" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := *base.Manifest.Runtime.RestoreDatabase
			p := base
			p.Manifest.Runtime.RestoreDatabase = &spec
			change(&p)
			if err := validateRestoreDatabaseSpec(p); err == nil {
				t.Fatal("unreviewed restore database stage accepted")
			}
		})
	}
}

func TestRestoreDatabaseJITConfinement(t *testing.T) {
	base, c := restoreFixture()
	for _, mode := range []string{"valid", "wrong-protocol", "wrong-reference", "count", "legacy-login", "missing-control"} {
		t.Run(mode, func(t *testing.T) {
			response := sdkclient.ServiceBindingCredentials{Protocol: sdkclient.ServiceBindingRestoreProtocol, Bindings: []sdkclient.ServiceBindingCredential{c}}
			switch mode {
			case "wrong-protocol":
				response.Protocol = sdkclient.ServiceBindingBackupProtocol
			case "wrong-reference":
				response.Bindings[0].Revision = strings.Repeat("9", 64)
			case "count":
				response.Bindings = nil
			case "legacy-login":
				response.Bindings[0].Username = c.Database
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != "POST" || r.URL.Path != "/v1/agent/service-binding-restores/"+base.DeploymentID || r.Header.Get("X-Agent-Id") != "agt_fixture" {
					t.Error("wrong restore credential transport")
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
			p := base
			p.ServiceBindingRetirementAuthorizations = []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: c.ServiceBindingRef}}
			cmd := &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandDeploy, ControlToken: "control"}
			if mode == "missing-control" {
				cmd.ControlToken = ""
			}
			spec := p.Manifest.Runtime.RestoreDatabase
			values, credential, err := (&Docker{Client: client}).prepareRestoreDatabase(context.Background(), cmd, &p, spec)
			if mode == "valid" {
				if err != nil || len(p.ServiceBindingRetirementAuthorizations) != 0 || len(values) != 2 || credential.Password != c.Password {
					t.Fatal("verified restore credential refused")
				}
				got, _ := p.Vars["DATABASE_URL"].(string)
				if !strings.Contains(got, c.Password) || len(serviceBindingEnvRedactions([]byte("DATABASE_URL="+got+"\n"))) != 2 {
					t.Fatal("injected credential escaped the runtime redaction contract")
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), c.Password) || len(p.Vars) != 1 {
				t.Fatal("unverified restore credential reached runtime or leaked")
			}
		})
	}
}

func TestRestoreDatabaseSQL(t *testing.T) {
	consumer, c := generationFixture()
	spec := sdkclient.RestoreDatabaseSpec{ServiceBindingRef: c.ServiceBindingRef, Protocol: sdkclient.ServiceBindingRestoreProtocol, ConsumerDeploymentID: consumer, RestoreDatabase: "imp_restore_" + strings.Repeat("8", 16)}
	create, err := postgresRestoreDatabaseSQL(consumer, c, spec.RestoreDatabase)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := postgresRestoreDropSQL(consumer, c, spec.RestoreDatabase)
	if err != nil {
		t.Fatal(err)
	}
	_, owner, _ := bindingGenerationNames(c.ServiceBindingRef)
	if !strings.Contains(create, "Restore owner cannot be verified") || !strings.Contains(create, "impreza-restore:"+c.BindingID) || !strings.Contains(create, "OWNER %I") || !strings.Contains(create, "already taken") || strings.Contains(create, c.Password) || strings.Contains(create, "DROP DATABASE") {
		t.Fatal("creation must only add a marked new database")
	}
	if !strings.Contains(drop, "DROP DATABASE IF EXISTS \""+spec.RestoreDatabase+"\"") || !strings.Contains(drop, "ownership cannot be verified") || strings.Contains(drop, c.Password) || !strings.Contains(drop, owner) {
		t.Fatal("drop must only remove a verified marked database")
	}
	for name, change := range map[string]func(*sdkclient.RestoreDatabaseSpec, *sdkclient.ServiceBindingCredential){
		"database shape": func(s *sdkclient.RestoreDatabaseSpec, c *sdkclient.ServiceBindingCredential) {
			s.RestoreDatabase = "postgres"
		},
		"admin injection": func(s *sdkclient.RestoreDatabaseSpec, c *sdkclient.ServiceBindingCredential) {
			c.AdminUser = "--command=bad"
		},
		"provider self": func(s *sdkclient.RestoreDatabaseSpec, c *sdkclient.ServiceBindingCredential) {
			c.ProviderDeploymentID = consumer
		},
	} {
		t.Run(name, func(t *testing.T) {
			badSpec, badCredential := spec, c
			change(&badSpec, &badCredential)
			if sql, err := postgresRestoreDatabaseSQL(consumer, badCredential, badSpec.RestoreDatabase); err == nil || sql != "" {
				t.Fatal("unverified restore identity produced SQL")
			}
			if sql, err := postgresRestoreDropSQL(consumer, badCredential, badSpec.RestoreDatabase); err == nil || sql != "" {
				t.Fatal("unverified restore identity produced drop SQL")
			}
		})
	}
}

func TestRestoreDatabaseWorkerPayload(t *testing.T) {
	base, _ := restoreFixture()
	wp := replacementPayload(base)
	if wp.Manifest.Runtime.RestoreDatabase == nil || *wp.Manifest.Runtime.RestoreDatabase != *base.Manifest.Runtime.RestoreDatabase {
		t.Fatal("worker lost the reviewed restore database stage")
	}
	raw, err := json.Marshal(wp)
	if err != nil || !strings.Contains(string(raw), "restore_database") || !strings.Contains(string(raw), "rspl_") {
		t.Fatal("worker payload round-trip lost the stage")
	}
	var decoded sdkclient.DeployPayload
	if json.Unmarshal(raw, &decoded) != nil || decoded.Manifest.Runtime.RestoreDatabase == nil || decoded.RestorePlanID != base.RestorePlanID {
		t.Fatal("worker payload did not decode the stage")
	}
}
