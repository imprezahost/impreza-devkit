package executor

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Restore SQL never receives the serving login or membership of its owner role.
func postgresDatabaseJobCredential(c sdkclient.ServiceBindingCredential, name string) (sdkclient.ServiceBindingCredential, error) {
	if (!backupScratchPattern.MatchString(name) && !restoreDatabasePattern.MatchString(name)) || !bindingRevisionPattern.MatchString(c.Password) {
		return c, errors.New("invalid database job identity")
	}
	mac := hmac.New(sha256.New, []byte(c.Password))
	mac.Write([]byte("impreza-postgres-job-v2:" + c.BindingID + ":" + c.ProviderDeploymentID + ":" + name))
	c.Password = hex.EncodeToString(mac.Sum(nil))
	prefix := "ijv_"
	if restoreDatabasePattern.MatchString(name) {
		prefix = "ijr_"
	}
	c.Username = prefix + name[len(name)-16:]
	c.Database = name
	return c, nil
}

func postgresDatabaseJobURL(c sdkclient.ServiceBindingCredential) string {
	u := url.URL{Scheme: "postgresql", User: url.UserPassword(c.Username, c.Password), Host: "pg_" + c.ProviderDeploymentID + ":5432", Path: "/" + c.Database, RawQuery: "sslmode=disable"}
	return u.String()
}

// Creation refuses collisions, including leftovers requiring reconciliation.
// A host/process failure between CREATE DATABASE and its marker can leave an
// object for manual cleanup; no later invocation adopts an unmarked object.
func postgresDatabaseJobSQL(consumer string, c sdkclient.ServiceBindingCredential, name string, create, keep bool) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, owner, login := bindingGenerationNames(c.ServiceBindingRef)
	if c.Database != database || c.Username != login || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid database job binding identity")
	}
	job, err := postgresDatabaseJobCredential(c, name)
	if err != nil {
		return "", err
	}
	verifier, err := postgresBindingVerifier(job.Password)
	if err != nil {
		return "", err
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	marker := "impreza-backup-verify:" + identity
	label := "Backup verification"
	if restoreDatabasePattern.MatchString(name) {
		marker = "impreza-restore:" + identity
		label = "Restore"
	}
	replace := strings.NewReplacer("@OWNER@", owner, "@OWNER_MARKER@", "impreza-owner:"+identity, "@DB@", database, "@NAME@", name, "@LOGIN@", job.Username, "@MARKER@", marker, "@VERIFIER@", verifier, "@LABEL@", label)
	sql := `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('@OWNER_MARKER@',0));
`
	if create {
		sql += `BEGIN;
DO $impreza$
BEGIN
 IF NOT EXISTS (SELECT FROM pg_authid r WHERE r.rolname='@OWNER@' AND NOT r.rolcanlogin AND r.rolpassword IS NULL
 AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
 AND shobj_description(r.oid,'pg_authid')='@OWNER_MARKER@') THEN RAISE EXCEPTION '@LABEL@ owner cannot be verified'; END IF;
 IF EXISTS (SELECT FROM pg_database WHERE datname='@NAME@') OR EXISTS (SELECT FROM pg_authid WHERE rolname='@LOGIN@') THEN
 RAISE EXCEPTION 'Database job name is already taken'; END IF;
END
$impreza$;
CREATE ROLE "@LOGIN@" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 4 PASSWORD '@VERIFIER@';
COMMENT ON ROLE "@LOGIN@" IS '@MARKER@';
COMMIT;
SELECT format('CREATE DATABASE %I OWNER %I','@NAME@','@OWNER@')
\gexec
COMMENT ON DATABASE "@NAME@" IS '@MARKER@';
REVOKE ALL ON DATABASE "@NAME@" FROM PUBLIC;
GRANT CONNECT,CREATE,TEMPORARY ON DATABASE "@NAME@" TO "@LOGIN@";
\connect @NAME@
GRANT USAGE,CREATE ON SCHEMA public TO "@LOGIN@";
`
	} else {
		sql += `DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_database WHERE datname='@NAME@') AND NOT EXISTS (
 SELECT FROM pg_database d JOIN pg_authid r ON r.oid=d.datdba WHERE d.datname='@NAME@' AND r.rolname='@OWNER@'
 AND shobj_description(d.oid,'pg_database')='@MARKER@') THEN RAISE EXCEPTION 'Database job ownership cannot be verified'; END IF;
 IF EXISTS (SELECT FROM pg_authid WHERE rolname='@LOGIN@') AND NOT EXISTS (SELECT FROM pg_authid r
 WHERE r.rolname='@LOGIN@' AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
 AND shobj_description(r.oid,'pg_authid')='@MARKER@'
 AND NOT EXISTS (SELECT FROM pg_auth_members m WHERE m.member=r.oid OR m.roleid=r.oid)) THEN
 RAISE EXCEPTION 'Database job login ownership cannot be verified'; END IF;
END
$impreza$;
SELECT format('ALTER ROLE %I NOLOGIN','@LOGIN@') WHERE EXISTS (SELECT FROM pg_authid WHERE rolname='@LOGIN@')
\gexec
SELECT pg_terminate_backend(pid,5000) FROM pg_stat_activity WHERE usename='@LOGIN@' AND pid<>pg_backend_pid();
`
		if keep {
			sql += `\connect @NAME@
SELECT format('REASSIGN OWNED BY %I TO %I','@LOGIN@','@OWNER@') WHERE EXISTS (SELECT FROM pg_roles WHERE rolname='@LOGIN@')
\gexec
SELECT format('DROP OWNED BY %I','@LOGIN@') WHERE EXISTS (SELECT FROM pg_roles WHERE rolname='@LOGIN@')
\gexec
\connect postgres
`
		} else {
			sql += `DROP DATABASE IF EXISTS "@NAME@";
`
		}
		sql += `DROP ROLE IF EXISTS "@LOGIN@";
`
	}
	return replace.Replace(sql), nil
}
