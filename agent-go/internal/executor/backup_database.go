package executor

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var backupJobPattern = regexp.MustCompile(`^bkpjob_[a-f0-9]{16}$`)
var backupScratchPattern = regexp.MustCompile(`^imp_verify_[a-f0-9]{16}$`)

// The reviewed database stage of a backup transport job. Everything is checked
// before a credential is fetched or the scratch database is created; the stage
// exists only inside a backup job payload and never mixes with the binding
// lifecycle manifests.
func validateBackupDatabaseSpec(p sdkclient.DeployPayload) error {
	runtime := p.Manifest.Runtime
	spec := runtime.BackupDatabase
	if spec == nil {
		return nil
	}
	if !backupJobPattern.MatchString(p.DeploymentID) || runtime.Type != "docker-compose" || runtime.Build != nil ||
		spec.Protocol != sdkclient.ServiceBindingBackupProtocol ||
		runtime.ServiceBindingProtocol != "" || len(runtime.ServiceBindings) != 0 ||
		runtime.ServiceBindingRetirementProtocol != "" || len(runtime.ServiceBindingRetirements) != 0 ||
		runtime.ServiceBindingRotationProtocol != "" || runtime.ServiceBindingRotation != nil ||
		!backupScratchPattern.MatchString(spec.VerifyDatabase) ||
		!bindingDeploymentPattern.MatchString(spec.ConsumerDeploymentID) {
		return errors.New("unsupported backup database manifest")
	}
	if err := validateServiceBindingRef(spec.ConsumerDeploymentID, spec.ServiceBindingRef); err != nil {
		return err
	}
	if _, exists := p.Vars[spec.Variable]; exists {
		return errors.New("backup database would overwrite a runtime variable")
	}
	return nil
}

// The credential is fetched just in time and only ever lands in the job's
// private .env (0600), never in the manifest, the result, or the logs. The
// credential services in the stack reach only the binding network.
func (d *Docker) prepareBackupDatabase(ctx context.Context, cmd *sdkclient.PollCommand, p *sdkclient.DeployPayload, spec *sdkclient.BackupDatabaseSpec) (map[string]string, sdkclient.ServiceBindingCredential, error) {
	var empty sdkclient.ServiceBindingCredential
	// A queued payload cannot carry authority into this stage.
	p.ServiceBindingRetirementAuthorizations = nil
	if err := validateBackupDatabaseSpec(*p); err != nil {
		return nil, empty, err
	}
	if d.Client == nil || cmd.Kind != sdkclient.CommandDeploy || cmd.ControlToken == "" {
		return nil, empty, errors.New("backup database requires an authenticated controlled deploy")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := d.Client.AgentServiceBindingBackup(fetchCtx, p.DeploymentID, cmd.ID, cmd.ControlToken)
	if err != nil || response == nil || response.Protocol != spec.Protocol || len(response.Bindings) != 1 || response.Bindings[0].ServiceBindingRef != spec.ServiceBindingRef {
		return nil, empty, errors.New("backup database credential does not match the reviewed operation")
	}
	credential := response.Bindings[0]
	database, _, login := bindingGenerationNames(credential.ServiceBindingRef)
	if credential.Username != login || credential.Database != database || !bindingRevisionPattern.MatchString(credential.Password) || !bindingAdminPattern.MatchString(credential.AdminUser) {
		return nil, empty, errors.New("invalid backup database credential shape")
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

// The scratch database proves the dump restores before the backup is reported.
// It is created and dropped only through the verified provider channel: the
// credential can never create or drop databases itself. The marker comment is
// what lets the drop path distinguish our scratch from anything else.
func postgresBackupScratchSQL(consumer string, c sdkclient.ServiceBindingCredential, scratch string) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, owner, _ := bindingGenerationNames(c.ServiceBindingRef)
	if c.Database != database || !backupScratchPattern.MatchString(scratch) || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid backup verification identity")
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	ownerMarker := "impreza-owner:" + identity
	marker := "impreza-backup-verify:" + identity
	return `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('` + ownerMarker + `',0));
BEGIN;
DO $impreza$
BEGIN
 IF NOT EXISTS (SELECT FROM pg_authid r WHERE r.rolname='` + owner + `' AND NOT r.rolcanlogin AND r.rolpassword IS NULL
  AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
  AND shobj_description(r.oid,'pg_authid')='` + ownerMarker + `') THEN
  RAISE EXCEPTION 'Backup verification owner cannot be verified'; END IF;
 IF EXISTS (SELECT FROM pg_database WHERE datname='` + scratch + `') AND NOT EXISTS (
  SELECT FROM pg_database d JOIN pg_authid r ON r.oid=d.datdba
  WHERE d.datname='` + scratch + `' AND r.rolname='` + owner + `' AND shobj_description(d.oid,'pg_database')='` + marker + `'
 ) THEN RAISE EXCEPTION 'Backup verification database cannot be adopted'; END IF;
END
$impreza$;
COMMIT;
SELECT format('CREATE DATABASE %I OWNER %I','` + scratch + `','` + owner + `') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='` + scratch + `')
\gexec
COMMENT ON DATABASE "` + scratch + `" IS '` + marker + `';
REVOKE ALL ON DATABASE "` + scratch + `" FROM PUBLIC;
`, nil
}

func postgresBackupScratchDropSQL(consumer string, c sdkclient.ServiceBindingCredential, scratch string) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, owner, _ := bindingGenerationNames(c.ServiceBindingRef)
	if c.Database != database || !backupScratchPattern.MatchString(scratch) || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid backup verification identity")
	}
	marker := "impreza-backup-verify:" + c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	return `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('impreza-owner:` + c.BindingID + `:` + c.ProviderDeploymentID + `:` + consumer + `',0));
DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_database WHERE datname='` + scratch + `') AND NOT EXISTS (
  SELECT FROM pg_database d JOIN pg_authid r ON r.oid=d.datdba
  WHERE d.datname='` + scratch + `' AND r.rolname='` + owner + `' AND shobj_description(d.oid,'pg_database')='` + marker + `'
 ) THEN RAISE EXCEPTION 'Backup verification database ownership cannot be verified'; END IF;
END
$impreza$;
SELECT pg_terminate_backend(pid,5000) FROM pg_stat_activity WHERE datname='` + scratch + `' AND pid<>pg_backend_pid();
DROP DATABASE IF EXISTS "` + scratch + `";
`, nil
}

// Verified provider channel, same shape as the retirement path: inspect the
// provider, prove its identity, then run the statement as its administrator.
func (d *Docker) runPostgresBackupSQL(ctx context.Context, c sdkclient.ServiceBindingCredential, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return d.retirePostgresBindingSQL(ctx, sdkclient.ServiceBindingRetirement{ServiceBindingRef: c.ServiceBindingRef, AdminUser: c.AdminUser}, "postgres", sql)
}

func (d *Docker) createPostgresBackupScratch(ctx context.Context, spec *sdkclient.BackupDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := postgresBackupScratchSQL(spec.ConsumerDeploymentID, c, spec.VerifyDatabase)
	if err != nil {
		return err
	}
	return d.runPostgresBackupSQL(ctx, c, sql)
}

func (d *Docker) dropPostgresBackupScratch(ctx context.Context, spec *sdkclient.BackupDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := postgresBackupScratchDropSQL(spec.ConsumerDeploymentID, c, spec.VerifyDatabase)
	if err != nil {
		return err
	}
	return d.runPostgresBackupSQL(ctx, c, sql)
}
