package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var restoreJobPattern = regexp.MustCompile(`^rstjob_[a-f0-9]{16}$`)
var restoreDatabasePattern = regexp.MustCompile(`^imp_restore_[a-f0-9]{16}$`)

// The reviewed database stage of a restore transport job. Everything is
// checked before a credential is fetched or the new database is created; the
// stage exists only inside a restore job payload and never mixes with the
// binding lifecycle manifests.
func validateRestoreDatabaseSpec(p sdkclient.DeployPayload) error {
	runtime := p.Manifest.Runtime
	spec := runtime.RestoreDatabase
	if spec == nil {
		return nil
	}
	if !restoreJobPattern.MatchString(p.DeploymentID) || runtime.Type != "docker-compose" || runtime.Build != nil ||
		spec.Protocol != sdkclient.ServiceBindingRestoreProtocol ||
		runtime.ServiceBindingProtocol != "" || len(runtime.ServiceBindings) != 0 ||
		runtime.ServiceBindingRetirementProtocol != "" || len(runtime.ServiceBindingRetirements) != 0 ||
		runtime.ServiceBindingRotationProtocol != "" || runtime.ServiceBindingRotation != nil ||
		runtime.BackupDatabase != nil ||
		!restoreDatabasePattern.MatchString(spec.RestoreDatabase) ||
		!bindingDeploymentPattern.MatchString(spec.ConsumerDeploymentID) ||
		spec.ExpectedTables < 0 ||
		!bindingRevisionPattern.MatchString(spec.DumpSHA256) {
		return errors.New("unsupported restore database manifest")
	}
	if err := validateServiceBindingRef(spec.ConsumerDeploymentID, spec.ServiceBindingRef); err != nil {
		return err
	}
	if _, exists := p.Vars[spec.Variable]; exists {
		return errors.New("restore database would overwrite a runtime variable")
	}
	return nil
}

// The credential is fetched just in time and only ever lands in the job's
// private .env (0600), never in the manifest, the result, or the logs.
func (d *Docker) prepareRestoreDatabase(ctx context.Context, cmd *sdkclient.PollCommand, p *sdkclient.DeployPayload, spec *sdkclient.RestoreDatabaseSpec) (map[string]string, sdkclient.ServiceBindingCredential, error) {
	var empty sdkclient.ServiceBindingCredential
	// A queued payload cannot carry authority into this stage.
	p.ServiceBindingRetirementAuthorizations = nil
	if err := validateRestoreDatabaseSpec(*p); err != nil {
		return nil, empty, err
	}
	if d.Client == nil || cmd.Kind != sdkclient.CommandDeploy || cmd.ControlToken == "" {
		return nil, empty, errors.New("restore database requires an authenticated controlled deploy")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := d.Client.AgentServiceBindingRestore(fetchCtx, p.DeploymentID, cmd.ID, cmd.ControlToken)
	if err != nil || response == nil || response.Protocol != spec.Protocol || len(response.Bindings) != 1 || response.Bindings[0].ServiceBindingRef != spec.ServiceBindingRef {
		return nil, empty, errors.New("restore database credential does not match the reviewed operation")
	}
	credential := response.Bindings[0]
	database, _, login := bindingGenerationNames(credential.ServiceBindingRef)
	if credential.Username != login || credential.Database != database || !bindingRevisionPattern.MatchString(credential.Password) || !bindingAdminPattern.MatchString(credential.AdminUser) {
		return nil, empty, errors.New("invalid restore database credential shape")
	}
	u := url.URL{Scheme: "postgresql", User: url.UserPassword(credential.Username, credential.Password), Host: "pg_" + credential.ProviderDeploymentID + ":5432", Path: "/" + credential.Database, RawQuery: "sslmode=disable"}
	value := u.String()
	vars := make(map[string]any, len(p.Vars)+1)
	for k, v := range p.Vars {
		vars[k] = v
	}
	vars[spec.Variable] = value
	p.Vars = vars
	return map[string]string{"url": value, "password": credential.Password}, credential, nil
}

// The new database is created and dropped only through the verified provider
// channel — the same one the backup's scratch verification uses. The marker
// comment is what lets the drop path distinguish our restore from anything
// else, and it is also what the customer (or support) sees when listing
// databases on the provider.
func postgresRestoreDatabaseSQL(consumer string, c sdkclient.ServiceBindingCredential, name string) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	_, owner, _ := bindingGenerationNames(c.ServiceBindingRef)
	if !restoreDatabasePattern.MatchString(name) || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid restore database identity")
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	ownerMarker := "impreza-owner:" + identity
	marker := "impreza-restore:" + identity
	return `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('` + ownerMarker + `',0));
BEGIN;
DO $impreza$
BEGIN
 IF NOT EXISTS (SELECT FROM pg_authid r WHERE r.rolname='` + owner + `' AND NOT r.rolcanlogin AND r.rolpassword IS NULL
  AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
  AND shobj_description(r.oid,'pg_authid')='` + ownerMarker + `') THEN
  RAISE EXCEPTION 'Restore owner cannot be verified'; END IF;
 IF EXISTS (SELECT FROM pg_database WHERE datname='` + name + `') THEN
  RAISE EXCEPTION 'Restore database name is already taken'; END IF;
END
$impreza$;
COMMIT;
SELECT format('CREATE DATABASE %I OWNER %I','` + name + `','` + owner + `')
\gexec
COMMENT ON DATABASE "` + name + `" IS '` + marker + `';
REVOKE ALL ON DATABASE "` + name + `" FROM PUBLIC;
`, nil
}

func postgresRestoreDropSQL(consumer string, c sdkclient.ServiceBindingCredential, name string) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	_, owner, _ := bindingGenerationNames(c.ServiceBindingRef)
	if !restoreDatabasePattern.MatchString(name) || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid restore database identity")
	}
	marker := "impreza-restore:" + c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	return `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('impreza-owner:` + c.BindingID + `:` + c.ProviderDeploymentID + `:` + consumer + `',0));
DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_database WHERE datname='` + name + `') AND NOT EXISTS (
  SELECT FROM pg_database d JOIN pg_authid r ON r.oid=d.datdba
  WHERE d.datname='` + name + `' AND r.rolname='` + owner + `' AND shobj_description(d.oid,'pg_database')='` + marker + `'
 ) THEN RAISE EXCEPTION 'Restore database ownership cannot be verified'; END IF;
END
$impreza$;
SELECT pg_terminate_backend(pid,5000) FROM pg_stat_activity WHERE datname='` + name + `' AND pid<>pg_backend_pid();
DROP DATABASE IF EXISTS "` + name + `";
`, nil
}

// The verified provider channel, with output: same inspections as the
// retirement path, then the statement runs as the provider administrator.
func (d *Docker) runPostgresRestoreSQL(ctx context.Context, c sdkclient.ServiceBindingCredential, database, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var containers []bindingProviderContainer
	var networks []bindingProviderNetwork
	raw, err := limitedRuntimeOutput(d.dockerCmd(ctx, "inspect", "pg_"+c.ProviderDeploymentID), 1024*1024)
	if err != nil || json.Unmarshal(raw, &containers) != nil {
		return "", errors.New("service provider container unavailable for the restore")
	}
	raw, err = limitedRuntimeOutput(d.dockerCmd(ctx, "network", "inspect", c.ProviderDeploymentID+"_default"), 1024*1024)
	if err != nil || json.Unmarshal(raw, &networks) != nil {
		return "", errors.New("service provider network unavailable for the restore")
	}
	if err = verifyBindingProvider(c.ProviderDeploymentID, containers, networks); err != nil {
		return "", err
	}
	if _, err = bindingProviderAddress(containers[0], networks[0]); err != nil {
		return "", err
	}
	cmd := d.dockerCmd(ctx, "exec", "-i", "--user", "postgres", containers[0].ID, "psql", "--no-psqlrc", "--username", c.AdminUser, "--dbname", database, "--quiet", "--tuples-only", "--no-align")
	cmd.Stdin = strings.NewReader(sql)
	out, err := limitedRuntimeOutput(cmd, 64*1024)
	if err != nil {
		return "", errors.New("restore database statement could not be verified")
	}
	return string(out), nil
}

func (d *Docker) createPostgresRestoreDatabase(ctx context.Context, spec *sdkclient.RestoreDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := postgresRestoreDatabaseSQL(spec.ConsumerDeploymentID, c, spec.RestoreDatabase)
	if err != nil {
		return err
	}
	_, err = d.runPostgresRestoreSQL(ctx, c, "postgres", sql)
	return err
}

func (d *Docker) dropPostgresRestoreDatabase(ctx context.Context, spec *sdkclient.RestoreDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := postgresRestoreDropSQL(spec.ConsumerDeploymentID, c, spec.RestoreDatabase)
	if err != nil {
		return err
	}
	_, err = d.runPostgresRestoreSQL(ctx, c, "postgres", sql)
	return err
}

// The outcome's table count comes from the provider itself, never from the
// job container's self-report.
func (d *Docker) countPostgresRestoreTables(ctx context.Context, spec *sdkclient.RestoreDatabaseSpec, c sdkclient.ServiceBindingCredential) (int, error) {
	if err := validateServiceBindingRef(spec.ConsumerDeploymentID, c.ServiceBindingRef); err != nil {
		return 0, err
	}
	if !restoreDatabasePattern.MatchString(spec.RestoreDatabase) {
		return 0, errors.New("invalid restore database identity")
	}
	out, err := d.runPostgresRestoreSQL(ctx, c, spec.RestoreDatabase,
		"SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema');\n")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("restored table count is not a number: %q", strings.TrimSpace(out))
	}
	return n, nil
}
