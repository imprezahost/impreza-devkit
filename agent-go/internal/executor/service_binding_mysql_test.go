package executor

import (
	"encoding/json"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func mysqlGenerationFixture() (string, sdkclient.ServiceBindingCredential) {
	consumer, c := generationFixture()
	c.AdminUser = "root"
	return consumer, c
}

func TestMysqlBindingVerifier(t *testing.T) {
	password := strings.Repeat("e", 64)
	verifier, err := mysqlBindingVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	// mysql_native_password: "*" + uppercase hex of SHA1(SHA1(password)).
	if len(verifier) != 41 || verifier[0] != '*' || verifier != strings.ToUpper(verifier) {
		t.Fatalf("verifier shape invalid: %q", verifier)
	}
	if strings.Contains(verifier, password) {
		t.Fatal("verifier carries the plaintext password")
	}
	if _, err := mysqlBindingVerifier("not-hex"); err == nil {
		t.Fatal("non-hex password accepted")
	}
}

func TestMysqlGenerationSQL(t *testing.T) {
	consumer, c := mysqlGenerationFixture()
	sql, err := mysqlGenerationSQL(consumer, c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, c.Password) {
		t.Fatal("provisioning SQL carries the plaintext password")
	}
	for _, want := range []string{
		"GET_LOCK('impreza-owner:",
		"CREATE USER IF NOT EXISTS '" + c.Username + "'@'%' IDENTIFIED VIA mysql_native_password USING '*",
		"GRANT ALL PRIVILEGES ON `" + c.Database + "`.* TO '" + c.Username + "'@'%'",
		"Generation database cannot be adopted",
		"Generation login cannot be adopted",
		"Generation login holds global privileges",
		"Generation login grant drift",
		"impreza_ownership",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("provisioning SQL is missing %q", want)
		}
	}
	// The grant is scoped to the binding database only.
	if strings.Contains(sql, "ON *.*") {
		t.Fatal("provisioning SQL grants global privileges")
	}
	for name, change := range map[string]func(*sdkclient.ServiceBindingCredential){
		"binding SQL":       func(c *sdkclient.ServiceBindingCredential) { c.BindingID = "bnd_'; DROP USER root; --" },
		"provider path":     func(c *sdkclient.ServiceBindingCredential) { c.ProviderDeploymentID = "../../other" },
		"self reference":    func(c *sdkclient.ServiceBindingCredential) { c.ProviderDeploymentID = consumer },
		"database adoption": func(c *sdkclient.ServiceBindingCredential) { c.Database = "mysql" },
		"user adoption":     func(c *sdkclient.ServiceBindingCredential) { c.Username = "root" },
		"password SQL":      func(c *sdkclient.ServiceBindingCredential) { c.Password = "';DROP USER root;--" },
		"admin option":      func(c *sdkclient.ServiceBindingCredential) { c.AdminUser = "--command=bad" },
		"revision":          func(c *sdkclient.ServiceBindingCredential) { c.Revision = "changed" },
		"variable newline":  func(c *sdkclient.ServiceBindingCredential) { c.Variable = "DATABASE_URL\nSECRET" },
	} {
		t.Run(name, func(t *testing.T) {
			c := c
			change(&c)
			if sql, err := mysqlGenerationSQL(consumer, c); err == nil || sql != "" {
				t.Fatal("unsafe binding produced SQL")
			}
		})
	}
}

func TestMysqlGenerationRetireSQL(t *testing.T) {
	consumer, c := mysqlGenerationFixture()
	retirement := sdkclient.ServiceBindingRetirement{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}
	sql, err := mysqlGenerationRetireSQL(consumer, retirement)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GET_LOCK('impreza-owner:",
		"KILL ",
		"REVOKE ALL PRIVILEGES, GRANT OPTION FROM '" + c.Username + "'@'%'",
		"DROP USER '" + c.Username + "'@'%'",
		"DELETE FROM `impreza_ownership`.`bindings`",
		"Generation retirement login cannot be verified",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("retirement SQL is missing %q", want)
		}
	}
	// MariaDB retirement drops the login — the database is never touched.
	if strings.Contains(sql, "DROP DATABASE") || strings.Contains(sql, "DELETE FROM `impreza_ownership`.`bindings` WHERE `name`='db:") {
		t.Fatal("retirement touches the database or its data")
	}
	retirement.Username = "root"
	if sql, err := mysqlGenerationRetireSQL(consumer, retirement); err == nil || sql != "" {
		t.Fatal("foreign retirement identity produced SQL")
	}
}

func TestMysqlRuntimePreservation(t *testing.T) {
	consumer, c := mysqlGenerationFixture()
	alias := "impreza-binding-" + strings.TrimPrefix(c.BindingID, "bnd_")
	value := "mysql://" + c.Username + ":" + c.Password + "@mariadb_" + c.ProviderDeploymentID + ":3306/" + c.Database
	model := func(url string) []byte {
		raw, err := json.Marshal(map[string]any{"networks": map[string]any{alias: map[string]any{"external": true, "name": c.ProviderDeploymentID + "_default"}}, "services": map[string]any{"app": map[string]any{"networks": map[string]any{alias: nil}, "environment": map[string]any{"DATABASE_URL": url}}}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	vars, err := preservedServiceBindingVars(consumer, model(value), map[string]any{"DOMAIN_URL": "https://next.example.test"})
	if err != nil || vars["DATABASE_URL"] != value {
		t.Fatal("route update lost the managed MariaDB credential")
	}
	for _, bad := range []string{
		strings.Replace(value, "mysql://", "postgresql://", 1),
		strings.Replace(value, "mariadb_"+c.ProviderDeploymentID, "pg_"+c.ProviderDeploymentID, 1),
		strings.Replace(value, ":3306", ":5432", 1),
		strings.Replace(value, c.Username, "imp_"+strings.TrimPrefix(c.BindingID, "bnd_"), 1),
		value + "?sslmode=disable",
	} {
		if _, err := preservedServiceBindingVars(consumer, model(bad), nil); err == nil {
			t.Fatalf("foreign MariaDB credential shape was preserved: %s", bad)
		}
	}
	redactions := serviceBindingEnvRedactions([]byte("DATABASE_URL=" + value + "\nKEEP=value\n"))
	clean := redactServiceBindingText(value+" "+c.Password, redactions)
	if len(redactions) != 2 || strings.Contains(clean, c.Password) || strings.Contains(clean, value) {
		t.Fatal("MariaDB URL/password leaked through runtime log redaction")
	}
}

func TestMysqlManifestValidation(t *testing.T) {
	consumer, c := mysqlGenerationFixture()
	base := func() sdkclient.DeployPayload {
		var p sdkclient.DeployPayload
		p.DeploymentID = consumer
		p.Manifest.Runtime.Type = "docker-compose"
		p.Vars = map[string]any{"DEPLOYMENT_ID": consumer}
		return p
	}
	p := base()
	p.Manifest.Runtime.ServiceBindingProtocol = sdkclient.MysqlServiceBindingGenerationProtocol
	p.Manifest.Runtime.ServiceBindings = []sdkclient.ServiceBindingRef{c.ServiceBindingRef}
	if err := validateServiceBindingManifest(p); err != nil {
		t.Fatal("mysql generation manifest refused")
	}
	p.Vars["DATABASE_URL"] = "mysql://override"
	if err := validateServiceBindingManifest(p); err == nil {
		t.Fatal("mysql binding could overwrite a runtime variable")
	}

	r := base()
	r.Manifest.Runtime.ServiceBindingRetirementProtocol = sdkclient.MysqlServiceBindingGenerationRetirementProtocol
	r.Manifest.Runtime.ServiceBindingRetirements = []sdkclient.ServiceBindingRef{c.ServiceBindingRef}
	if err := validateServiceBindingManifest(r); err != nil {
		t.Fatal("mysql retirement manifest refused")
	}
	// The engines never mix: a mysql attach with a postgres retirement.
	mixed := base()
	mixed.Manifest.Runtime.ServiceBindingRotationProtocol = sdkclient.ServiceBindingRotationProtocol
	mixed.Manifest.Runtime.ServiceBindingProtocol = sdkclient.MysqlServiceBindingGenerationProtocol
	mixed.Manifest.Runtime.ServiceBindingRotation = &sdkclient.ServiceBindingRotationIntent{RotationID: "brot_" + strings.Repeat("1", 24), Mode: "rotate", Previous: c.ServiceBindingRef, Candidate: c.ServiceBindingRef}
	mixed.Manifest.Runtime.ServiceBindingRotation.Candidate.Revision = strings.Repeat("2", 64)
	mixed.Manifest.Runtime.ServiceBindings = []sdkclient.ServiceBindingRef{mixed.Manifest.Runtime.ServiceBindingRotation.Candidate}
	mixed.Manifest.Runtime.Startup = &sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: 60}
	if err := validateServiceBindingManifest(mixed); err == nil {
		t.Fatal("a cross-engine rotation manifest was accepted")
	}
}
