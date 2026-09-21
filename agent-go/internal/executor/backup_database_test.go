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

func backupFixture() (sdkclient.DeployPayload, sdkclient.ServiceBindingCredential) {
	consumer, c := generationFixture()
	spec := sdkclient.BackupDatabaseSpec{
		ServiceBindingRef:    c.ServiceBindingRef,
		Protocol:             sdkclient.ServiceBindingBackupProtocol,
		ConsumerDeploymentID: consumer,
		VerifyDatabase:       "imp_verify_" + strings.Repeat("7", 16),
	}
	p := sdkclient.DeployPayload{
		DeploymentID: "bkpjob_" + strings.Repeat("4", 16),
		Vars:         map[string]any{"IMPREZA_JOB": "bkp_" + strings.Repeat("3", 8)},
		Manifest:     sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", BackupDatabase: &spec}},
	}
	return p, c
}

func TestBackupDatabaseManifestBoundaries(t *testing.T) {
	base, c := backupFixture()
	if err := validateBackupDatabaseSpec(base); err != nil {
		t.Fatal(err)
	}
	plain := base
	plain.Manifest.Runtime.BackupDatabase = nil
	if err := validateBackupDatabaseSpec(plain); err != nil {
		t.Fatal("ordinary backup job refused")
	}
	for name, change := range map[string]func(*sdkclient.DeployPayload){
		"protocol": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.BackupDatabase.Protocol = sdkclient.ServiceBindingGenerationProtocol
		},
		"deployment":   func(p *sdkclient.DeployPayload) { p.DeploymentID = "dpl_" + strings.Repeat("c", 16) },
		"build":        func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Build = &sdkclient.BuildContext{} },
		"runtime":      func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.Type = "docker" },
		"scratch name": func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.BackupDatabase.VerifyDatabase = "imp_main" },
		"consumer": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.BackupDatabase.ConsumerDeploymentID = "bkpjob_" + strings.Repeat("4", 16)
		},
		"self provider": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.BackupDatabase.ProviderDeploymentID = p.Manifest.Runtime.BackupDatabase.ConsumerDeploymentID
		},
		"binding mixed": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindings = []sdkclient.ServiceBindingRef{c.ServiceBindingRef}
			p.Manifest.Runtime.ServiceBindingProtocol = sdkclient.ServiceBindingGenerationProtocol
		},
		"rotation mixed": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.ServiceBindingRotationProtocol = sdkclient.ServiceBindingRotationProtocol
		},
		"variable":          func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.BackupDatabase.Variable = "PGPASSWORD" },
		"variable occupied": func(p *sdkclient.DeployPayload) { p.Vars["DATABASE_URL"] = "untrusted" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := *base.Manifest.Runtime.BackupDatabase
			p := base
			p.Manifest.Runtime.BackupDatabase = &spec
			change(&p)
			if err := validateBackupDatabaseSpec(p); err == nil {
				t.Fatal("unreviewed backup database stage accepted")
			}
		})
	}
}

func TestBackupDatabaseJITConfinement(t *testing.T) {
	base, c := backupFixture()
	for _, mode := range []string{"valid", "wrong-protocol", "wrong-reference", "count", "legacy-login", "missing-control"} {
		t.Run(mode, func(t *testing.T) {
			response := sdkclient.ServiceBindingCredentials{Protocol: sdkclient.ServiceBindingBackupProtocol, Bindings: []sdkclient.ServiceBindingCredential{c}}
			switch mode {
			case "wrong-protocol":
				response.Protocol = sdkclient.ServiceBindingGenerationProtocol
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
				if r.Method != "POST" || r.URL.Path != "/v1/agent/service-binding-backup/"+base.DeploymentID || r.Header.Get("X-Agent-Id") != "agt_fixture" {
					t.Error("wrong backup credential transport")
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
			spec := p.Manifest.Runtime.BackupDatabase
			values, credential, err := (&Docker{Client: client}).prepareBackupDatabase(context.Background(), cmd, &p, spec)
			if mode == "valid" {
				if err != nil || len(p.ServiceBindingRetirementAuthorizations) != 0 || len(values) != 4 || credential.Password != c.Password {
					t.Fatal("verified backup credential refused")
				}
				got, _ := p.Vars["DATABASE_URL"].(string)
				if !strings.Contains(got, c.Password) || len(serviceBindingEnvRedactions([]byte("DATABASE_URL="+got+"\n"))) != 2 {
					t.Fatal("injected credential escaped the runtime redaction contract")
				}
				jobURL, _ := p.Vars["IMPREZA_DATABASE_JOB_URL"].(string)
				if strings.Contains(jobURL, c.Password) || !strings.Contains(jobURL, "ijv_") || len(serviceBindingEnvRedactions([]byte("IMPREZA_DATABASE_JOB_URL="+jobURL+"\n"))) != 2 {
					t.Fatal("verification borrowed serving credential or escaped redaction")
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), c.Password) || len(p.Vars) != 1 {
				t.Fatal("unverified backup credential reached runtime or leaked")
			}
		})
	}
}

func TestBackupDatabaseScratchSQL(t *testing.T) {
	consumer, c := generationFixture()
	spec := sdkclient.BackupDatabaseSpec{ServiceBindingRef: c.ServiceBindingRef, Protocol: sdkclient.ServiceBindingBackupProtocol, ConsumerDeploymentID: consumer, VerifyDatabase: "imp_verify_" + strings.Repeat("7", 16)}
	create, err := postgresBackupScratchSQL(consumer, c, spec.VerifyDatabase)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := postgresBackupScratchDropSQL(consumer, c, spec.VerifyDatabase)
	if err != nil {
		t.Fatal(err)
	}
	_, owner, _ := bindingGenerationNames(c.ServiceBindingRef)
	if !strings.Contains(create, "Backup verification owner cannot be verified") || !strings.Contains(create, "impreza-backup-verify:"+c.BindingID) || !strings.Contains(create, "OWNER %I") || strings.Contains(create, c.Password) || strings.Contains(create, "DROP DATABASE") {
		t.Fatal("creation must only add a marked scratch database")
	}
	if !strings.Contains(drop, "DROP DATABASE IF EXISTS \""+spec.VerifyDatabase+"\"") || !strings.Contains(drop, "ownership cannot be verified") || strings.Contains(drop, c.Password) || !strings.Contains(drop, owner) {
		t.Fatal("drop must only remove a verified marked scratch database")
	}
	for name, change := range map[string]func(*sdkclient.BackupDatabaseSpec, *sdkclient.ServiceBindingCredential){
		"scratch shape": func(s *sdkclient.BackupDatabaseSpec, c *sdkclient.ServiceBindingCredential) {
			s.VerifyDatabase = "postgres"
		},
		"database drift": func(s *sdkclient.BackupDatabaseSpec, c *sdkclient.ServiceBindingCredential) { c.Database = "postgres" },
		"admin injection": func(s *sdkclient.BackupDatabaseSpec, c *sdkclient.ServiceBindingCredential) {
			c.AdminUser = "--command=bad"
		},
		"provider self": func(s *sdkclient.BackupDatabaseSpec, c *sdkclient.ServiceBindingCredential) {
			c.ProviderDeploymentID = consumer
		},
	} {
		t.Run(name, func(t *testing.T) {
			badSpec, badCredential := spec, c
			change(&badSpec, &badCredential)
			if sql, err := postgresBackupScratchSQL(consumer, badCredential, badSpec.VerifyDatabase); err == nil || sql != "" {
				t.Fatal("unverified scratch identity produced SQL")
			}
			if sql, err := postgresBackupScratchDropSQL(consumer, badCredential, badSpec.VerifyDatabase); err == nil || sql != "" {
				t.Fatal("unverified scratch identity produced drop SQL")
			}
		})
	}
}

func TestBackupDatabaseWorkerPayload(t *testing.T) {
	base, _ := backupFixture()
	wp := replacementPayload(base)
	if wp.Manifest.Runtime.BackupDatabase == nil || *wp.Manifest.Runtime.BackupDatabase != *base.Manifest.Runtime.BackupDatabase {
		t.Fatal("worker lost the reviewed backup database stage")
	}
	if wp.Manifest.Runtime.ComposeYAML != "" {
		t.Fatal("worker gained source material")
	}
	raw, err := json.Marshal(wp)
	if err != nil || !strings.Contains(string(raw), "backup_database") {
		t.Fatal("worker payload round-trip lost the stage")
	}
	var decoded sdkclient.DeployPayload
	if json.Unmarshal(raw, &decoded) != nil || decoded.Manifest.Runtime.BackupDatabase == nil {
		t.Fatal("worker payload did not decode the stage")
	}
}
