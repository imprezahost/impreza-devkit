package executor

import (
	"context"
	"errors"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Private primitive: dispatch requires a separately verified healthy replacement.
// A retired login is retained as an identity tombstone and cannot be provisioned
// again. No database, table, role, or customer transaction is deleted here.
func postgresGenerationRetireSQL(consumer string, c sdkclient.ServiceBindingRetirement) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, owner, login := bindingGenerationNames(c.ServiceBindingRef)
	if c.Database != database || c.Username != login || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid generation retirement identity")
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	ownerMarker := "impreza-owner:" + identity
	marker := "impreza-generation:" + identity + ":" + c.Revision
	generationPrefix := "impreza-generation:" + identity + ":"
	loginPrefix := "ibg_" + c.BindingID[4:] + "_"
	// REASSIGN OWNED operates on the current database AND shared objects. Refuse
	// ownership outside this database instead of touching another application's data.
	guard := `DO $impreza$
BEGIN
 IF current_database()<>'` + database + `' OR NOT EXISTS (
  SELECT FROM pg_authid o JOIN pg_database d ON d.datdba=o.oid
  WHERE d.datname='` + database + `' AND o.rolname='` + owner + `'
   AND shobj_description(o.oid,'pg_authid')='` + ownerMarker + `'
   AND NOT o.rolcanlogin AND o.rolpassword IS NULL AND NOT o.rolsuper AND NOT o.rolcreatedb AND NOT o.rolcreaterole AND NOT o.rolreplication AND NOT o.rolbypassrls
   AND NOT EXISTS (SELECT FROM pg_auth_members WHERE member=o.oid)
   AND NOT EXISTS (SELECT FROM pg_database WHERE datdba=o.oid AND datname<>'` + database + `')
   AND NOT EXISTS (SELECT FROM pg_tablespace WHERE spcowner=o.oid)
 ) THEN RAISE EXCEPTION 'Generation retirement owner cannot be verified'; END IF;
 IF EXISTS (
  SELECT FROM pg_auth_members a JOIN pg_authid g ON g.oid=a.member
  WHERE a.roleid=(SELECT oid FROM pg_roles WHERE rolname='` + owner + `') AND (
   a.admin_option OR NOT a.inherit_option OR NOT a.set_option OR g.rolsuper OR g.rolcreatedb OR g.rolcreaterole OR g.rolreplication OR g.rolbypassrls
   OR COALESCE(shobj_description(g.oid,'pg_authid'),'') !~ '^` + generationPrefix + `[a-f0-9]{64}$'
   OR g.rolname<>'` + loginPrefix + `'||substring(shobj_description(g.oid,'pg_authid') from length('` + generationPrefix + `')+1 for 24)
   OR EXISTS (SELECT FROM pg_auth_members extra WHERE extra.member=g.oid AND extra.roleid<>a.roleid)
   OR EXISTS (SELECT FROM pg_auth_members extra WHERE extra.roleid=g.oid)
  )
 ) THEN RAISE EXCEPTION 'Generation retirement owner membership drift'; END IF;
 IF NOT EXISTS (
  SELECT FROM pg_authid g WHERE g.rolname='` + login + `' AND shobj_description(g.oid,'pg_authid')='` + marker + `'
   AND NOT g.rolsuper AND NOT g.rolcreatedb AND NOT g.rolcreaterole AND NOT g.rolreplication AND NOT g.rolbypassrls
   AND NOT EXISTS (SELECT FROM pg_auth_members WHERE roleid=g.oid)
   AND NOT EXISTS (SELECT FROM pg_auth_members a JOIN pg_roles o ON o.oid=a.roleid WHERE a.member=g.oid AND (o.rolname<>'` + owner + `' OR a.admin_option OR NOT a.inherit_option OR NOT a.set_option))
   AND ((NOT g.rolcanlogin AND g.rolpassword IS NULL) OR EXISTS (SELECT FROM pg_auth_members a JOIN pg_roles o ON o.oid=a.roleid WHERE a.member=g.oid AND o.rolname='` + owner + `'))
   AND NOT EXISTS (SELECT FROM pg_shdepend s WHERE s.refclassid='pg_authid'::regclass AND s.refobjid=g.oid AND s.deptype='o' AND s.dbid<>(SELECT oid FROM pg_database WHERE datname='` + database + `'))
 ) THEN RAISE EXCEPTION 'Generation retirement login cannot be verified'; END IF;
 IF EXISTS (SELECT FROM pg_prepared_xacts WHERE database='` + database + `') THEN
  RAISE EXCEPTION 'Prepared customer transaction prevents generation retirement';
 END IF;
END
$impreza$;
`
	return `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('` + ownerMarker + `',0));
BEGIN;
` + guard + `ALTER ROLE "` + login + `" NOLOGIN PASSWORD NULL;
COMMIT;
SELECT pg_terminate_backend(pid,5000) FROM pg_stat_activity WHERE usename='` + login + `' AND pid<>pg_backend_pid();
BEGIN;
` + guard + `DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_stat_activity WHERE usename='` + login + `') OR EXISTS (SELECT FROM pg_authid WHERE rolname='` + login + `' AND (rolcanlogin OR rolpassword IS NOT NULL)) THEN
  RAISE EXCEPTION 'Generation sessions are not quiescent';
 END IF;
END
$impreza$;
REASSIGN OWNED BY "` + login + `" TO "` + owner + `";
REVOKE "` + owner + `" FROM "` + login + `";
ALTER ROLE "` + login + `" IN DATABASE "` + database + `" RESET ALL;
COMMIT;
`, nil
}

func (d *Docker) retirePostgresGeneration(ctx context.Context, consumer string, c sdkclient.ServiceBindingRetirement) error {
	sql, err := postgresGenerationRetireSQL(consumer, c)
	if err != nil {
		return err
	}
	return d.retirePostgresBindingSQL(ctx, c, c.Database, sql)
}
