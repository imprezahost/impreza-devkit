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

// MariaDB engine's backup/restore database stages: the manifest boundaries,
// the registry-marker SQL shapes, and the URL the stack serves through.

func mysqlBackupFixture() (sdkclient.DeployPayload, sdkclient.ServiceBindingCredential) {
	consumer, c := generationFixture()
	spec := sdkclient.BackupDatabaseSpec{
		ServiceBindingRef:    c.ServiceBindingRef,
		Protocol:             sdkclient.MysqlServiceBindingBackupProtocol,
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

func TestMysqlBackupDatabaseManifestBoundaries(t *testing.T) {
	base, _ := mysqlBackupFixture()
	if err := validateBackupDatabaseSpec(base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*sdkclient.DeployPayload){
		"protocol": func(p *sdkclient.DeployPayload) {
			p.Manifest.Runtime.BackupDatabase.Protocol = sdkclient.MysqlServiceBindingGenerationProtocol
		},
		"postgres protocol on mysql job shape": func(p *sdkclient.DeployPayload) {
			// Not refused by itself: the protocol decides the engine. The
			// postgres protocol stays valid here; what must never validate is
			// a lifecycle or unknown protocol.
			p.Manifest.Runtime.BackupDatabase.Protocol = "mysql-service-binding-rotation-v1"
		},
		"deployment":        func(p *sdkclient.DeployPayload) { p.DeploymentID = "dpl_" + strings.Repeat("c", 16) },
		"scratch name":      func(p *sdkclient.DeployPayload) { p.Manifest.Runtime.BackupDatabase.VerifyDatabase = "imp_main" },
		"variable occupied": func(p *sdkclient.DeployPayload) { p.Vars["DATABASE_URL"] = "untrusted" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := *base.Manifest.Runtime.BackupDatabase
			p := base
			p.Manifest.Runtime.BackupDatabase = &spec
			change(&p)
			if err := validateBackupDatabaseSpec(p); err == nil {
				t.Fatal("unreviewed mysql backup stage accepted")
			}
		})
	}
}

func TestMysqlBackupScratchSQL(t *testing.T) {
	consumer, c := generationFixture()
	scratch := "imp_verify_" + strings.Repeat("7", 16)
	sql, err := mysqlBackupScratchSQL(consumer, c, scratch)
	if err != nil {
		t.Fatal(err)
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	for _, want := range []string{
		"GET_LOCK('impreza-owner:" + identity + "'",
		"'Database job binding ownership cannot be verified'",
		"'Database job name is already taken'",
		"CREATE DATABASE `" + scratch + "`",
		"('db:" + scratch + "','impreza-backup-verify:" + identity + "')",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("scratch SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, c.Password) {
		t.Fatal("credential boundary broken")
	}
	drop, err := mysqlBackupScratchDropSQL(consumer, c, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drop, "'Database job ownership cannot be verified'") ||
		!strings.Contains(drop, "DROP DATABASE IF EXISTS `"+scratch+"`") {
		t.Fatalf("drop SQL shape drifted:\n%s", drop)
	}
}

func mysqlRestoreFixtureSpec() (string, *sdkclient.RestoreDatabaseSpec, sdkclient.ServiceBindingCredential) {
	consumer, c := generationFixture()
	spec := &sdkclient.RestoreDatabaseSpec{
		ServiceBindingRef:    c.ServiceBindingRef,
		Protocol:             sdkclient.MysqlServiceBindingRestoreProtocol,
		ConsumerDeploymentID: consumer,
		RestoreDatabase:      "imp_restore_" + strings.Repeat("9", 16),
		ExpectedTables:       3,
		DumpSHA256:           strings.Repeat("a", 64),
	}
	return consumer, spec, c
}

func TestMysqlRestoreDatabaseManifestBoundaries(t *testing.T) {
	_, spec, _ := mysqlRestoreFixtureSpec()
	p := sdkclient.DeployPayload{
		DeploymentID: "rstjob_" + strings.Repeat("5", 16),
		Vars:         map[string]any{"IMPREZA_JOB": "rst_" + strings.Repeat("3", 8)},
		Manifest:     sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", RestoreDatabase: spec}},
	}
	if err := validateRestoreDatabaseSpec(p); err != nil {
		t.Fatal(err)
	}
	if err := validateRestoreDatabaseSpec(p); err == nil && spec.Protocol != sdkclient.MysqlServiceBindingRestoreProtocol {
		t.Fatal("protocol lost")
	}
	for name, change := range map[string]func(*sdkclient.RestoreDatabaseSpec, *sdkclient.DeployPayload){
		"unknown protocol": func(s *sdkclient.RestoreDatabaseSpec, _ *sdkclient.DeployPayload) {
			s.Protocol = "mysql-service-binding-v2"
		},
		"restore name":   func(s *sdkclient.RestoreDatabaseSpec, _ *sdkclient.DeployPayload) { s.RestoreDatabase = "imp_main" },
		"negative count": func(s *sdkclient.RestoreDatabaseSpec, _ *sdkclient.DeployPayload) { s.ExpectedTables = -1 },
		"dump sha":       func(s *sdkclient.RestoreDatabaseSpec, _ *sdkclient.DeployPayload) { s.DumpSHA256 = "zz" },
	} {
		t.Run(name, func(t *testing.T) {
			copied := *spec
			pp := sdkclient.DeployPayload{
				DeploymentID: p.DeploymentID,
				Vars:         map[string]any{"X": "1"},
				Manifest:     sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", RestoreDatabase: &copied}},
			}
			change(&copied, &pp)
			if err := validateRestoreDatabaseSpec(pp); err == nil {
				t.Fatal("unreviewed mysql restore stage accepted")
			}
		})
	}
}

func TestMysqlRestoreDatabaseSQL(t *testing.T) {
	consumer, spec, c := mysqlRestoreFixtureSpec()
	sql, err := mysqlRestoreDatabaseSQL(consumer, c, spec.RestoreDatabase)
	if err != nil {
		t.Fatal(err)
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	for _, want := range []string{
		"GET_LOCK('impreza-owner:" + identity + "'",
		"'Database job binding ownership cannot be verified'",
		"'Database job name is already taken'",
		"CREATE DATABASE `" + spec.RestoreDatabase + "`",
		"('db:" + spec.RestoreDatabase + "','impreza-restore:" + identity + "')",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("restore SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, c.Password) {
		t.Fatal("credential boundary broken")
	}
	drop, err := mysqlRestoreDropSQL(consumer, c, spec.RestoreDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drop, "'Database job ownership cannot be verified'") ||
		!strings.Contains(drop, "DROP DATABASE IF EXISTS `"+spec.RestoreDatabase+"`") {
		t.Fatalf("restore drop SQL drifted:\n%s", drop)
	}
}

func TestMysqlBackupRestoreURLs(t *testing.T) {
	_, c := generationFixture()
	bURL, err := mysqlBackupURL(c, c.Database)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bURL, "mysql://"+c.Username+":") ||
		!strings.Contains(bURL, "@mariadb_"+c.ProviderDeploymentID+":3306/"+c.Database) {
		t.Fatalf("backup URL drifted: %s", bURL)
	}
	if strings.Contains(bURL, "sslmode") {
		t.Fatal("mysql URL must not carry postgres options")
	}
	rURL, err := mysqlRestoreURL(c, c.Database)
	if err != nil {
		t.Fatal(err)
	}
	if rURL != bURL {
		t.Fatalf("restore URL should match the serving shape: %s vs %s", rURL, bURL)
	}
	bad := c
	bad.Password = "short"
	if _, err := mysqlBackupURL(bad, bad.Database); err == nil {
		t.Fatal("invalid credential accepted for URL")
	}
}

func TestMysqlJobCredentialsAreIsolatedAtJITEntry(t *testing.T) {
	for _, restore := range []bool{false, true} {
		p, c := mysqlBackupFixture()
		protocol := sdkclient.MysqlServiceBindingBackupProtocol
		name := p.Manifest.Runtime.BackupDatabase.VerifyDatabase
		if restore {
			_, spec, credential := mysqlRestoreFixtureSpec()
			c = credential
			p.Manifest.Runtime.BackupDatabase = nil
			p.Manifest.Runtime.RestoreDatabase = spec
			p.DeploymentID = "rstjob_" + strings.Repeat("5", 16)
			protocol = sdkclient.MysqlServiceBindingRestoreProtocol
			name = spec.RestoreDatabase
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": sdkclient.ServiceBindingCredentials{Protocol: protocol, Bindings: []sdkclient.ServiceBindingCredential{c}}})
		}))
		client, err := sdkclient.NewAgent(sdkclient.AgentOptions{AgentID: "agt_fixture", AgentSecret: "fixture", BaseURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		d := &Docker{Client: client}
		cmd := &sdkclient.PollCommand{ID: "cmd_fixture", Kind: sdkclient.CommandDeploy, ControlToken: "control"}
		var secrets map[string]string
		key := "IMPREZA_DATABASE_JOB_URL"
		if restore {
			secrets, _, err = d.prepareRestoreDatabase(context.Background(), cmd, &p, p.Manifest.Runtime.RestoreDatabase)
			key = "DATABASE_URL"
		} else {
			secrets, _, err = d.prepareBackupDatabase(context.Background(), cmd, &p, p.Manifest.Runtime.BackupDatabase)
		}
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		value, _ := p.Vars[key].(string)
		job, _ := mysqlDatabaseJobCredential(c, name)
		if !strings.Contains(value, job.Password) || strings.Contains(value, c.Password) || !strings.Contains(value, "/"+name) {
			t.Fatal("job credential was not separated from the serving login")
		}
		if strings.Contains(redactServiceBindingText(value, secrets), job.Password) || redactServiceBindingText(job.Password, secrets) != "[redacted]" {
			t.Fatal("runtime secret redaction missed the job credential")
		}
		if len(serviceBindingEnvRedactions([]byte(key+"="+value+"\n"))) != 2 {
			t.Fatal("persisted job environment cannot be redacted on recovery")
		}
	}
}
