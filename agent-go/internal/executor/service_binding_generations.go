package executor

import (
	"context"
	"errors"
	"strconv"
	"strings"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Private generation foundation. No dispatcher or public capability uses this
// protocol until its complete reviewed lifecycle has passed acceptance.
func bindingGenerationNames(ref sdkclient.ServiceBindingRef) (database, owner, login string) {
	id := strings.TrimPrefix(ref.BindingID, "bnd_")
	database, owner = "imp_"+id, "ibo_"+id
	if len(ref.Revision) >= 24 {
		login = "ibg_" + id + "_" + ref.Revision[:24]
	}
	return
}

func postgresGenerationSQL(consumer string, c sdkclient.ServiceBindingCredential) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, owner, login := bindingGenerationNames(c.ServiceBindingRef)
	if c.Username != login || c.Database != database || !bindingRevisionPattern.MatchString(c.Password) || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid service credential generation")
	}
	verifier, err := postgresBindingVerifier(c.Password)
	if err != nil {
		return "", err
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	ownerMarker := "impreza-owner:" + identity
	generationPrefix := "impreza-generation:" + identity + ":"
	marker := generationPrefix + c.Revision
	loginPrefix := "ibg_" + strings.TrimPrefix(c.BindingID, "bnd_") + "_"
	// All substituted names/markers are derived from strict identifier/hex
	// grammars. Only the SCRAM verifier, never plaintext, enters SQL.
	sql := `\set ON_ERROR_STOP on
SELECT pg_advisory_lock(hashtextextended('` + ownerMarker + `',0));
BEGIN;
DO $impreza$
BEGIN
 IF EXISTS (SELECT FROM pg_roles WHERE rolname='` + login + `') AND NOT EXISTS (
  SELECT FROM pg_authid g JOIN pg_auth_members a ON a.member=g.oid JOIN pg_authid o ON o.oid=a.roleid JOIN pg_database d ON d.datdba=o.oid
  WHERE g.rolname='` + login + `' AND g.rolcanlogin AND shobj_description(g.oid,'pg_authid')='` + marker + `'
   AND o.rolname='` + owner + `' AND shobj_description(o.oid,'pg_authid')='` + ownerMarker + `' AND d.datname='` + database + `'
   AND NOT a.admin_option AND a.inherit_option AND a.set_option
 ) THEN RAISE EXCEPTION 'Existing generation cannot be adopted'; END IF;
 IF EXISTS (SELECT FROM pg_database WHERE datname='` + database + `') AND NOT EXISTS (
  SELECT FROM pg_database d JOIN pg_authid r ON r.oid=d.datdba
  WHERE d.datname='` + database + `' AND r.rolname='` + owner + `' AND shobj_description(r.oid,'pg_authid')='` + ownerMarker + `'
 ) THEN RAISE EXCEPTION 'Generation database ownership cannot be verified'; END IF;
 IF EXISTS (SELECT FROM pg_roles WHERE rolname='` + owner + `') THEN
  IF NOT EXISTS (SELECT FROM pg_authid r WHERE r.rolname='` + owner + `' AND NOT r.rolcanlogin AND r.rolpassword IS NULL
   AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
   AND shobj_description(r.oid,'pg_authid')='` + ownerMarker + `'
   AND NOT EXISTS (SELECT FROM pg_auth_members WHERE member=r.oid)
   AND NOT EXISTS (SELECT FROM pg_database WHERE datdba=r.oid AND datname<>'` + database + `')
   AND NOT EXISTS (SELECT FROM pg_tablespace WHERE spcowner=r.oid)
  ) THEN RAISE EXCEPTION 'Generation owner role cannot be verified'; END IF;
 ELSE
  CREATE ROLE "` + owner + `" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD NULL;
  COMMENT ON ROLE "` + owner + `" IS '` + ownerMarker + `';
 END IF;
 IF EXISTS (
  SELECT FROM pg_auth_members a JOIN pg_authid r ON r.oid=a.member
  WHERE a.roleid=(SELECT oid FROM pg_roles WHERE rolname='` + owner + `') AND (
   a.admin_option OR NOT a.inherit_option OR NOT a.set_option OR r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls
   OR COALESCE(shobj_description(r.oid,'pg_authid'),'') !~ '^` + generationPrefix + `[a-f0-9]{64}$'
   OR r.rolname <> '` + loginPrefix + `'||substring(shobj_description(r.oid,'pg_authid') from ` + strconv.Itoa(len(generationPrefix)+1) + ` for 24)
   OR EXISTS (SELECT FROM pg_auth_members extra WHERE extra.member=r.oid AND extra.roleid<>a.roleid)
   OR EXISTS (SELECT FROM pg_auth_members extra WHERE extra.roleid=r.oid)
  )
 ) THEN RAISE EXCEPTION 'Generation owner membership drift'; END IF;
END
$impreza$;
COMMIT;
SELECT format('CREATE DATABASE %I OWNER %I','` + database + `','` + owner + `') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='` + database + `')
\gexec
BEGIN;
REVOKE ALL ON DATABASE "` + database + `" FROM PUBLIC;
DO $impreza$
BEGIN
 IF NOT EXISTS (SELECT FROM pg_database d JOIN pg_authid r ON r.oid=d.datdba WHERE d.datname='` + database + `' AND r.rolname='` + owner + `' AND shobj_description(r.oid,'pg_authid')='` + ownerMarker + `') THEN
  RAISE EXCEPTION 'Generation database ownership changed';
 END IF;
 IF EXISTS (SELECT FROM pg_roles WHERE rolname='` + login + `') THEN
  IF NOT EXISTS (SELECT FROM pg_authid r WHERE r.rolname='` + login + `' AND r.rolcanlogin AND r.rolpassword IS NOT NULL
   AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
   AND shobj_description(r.oid,'pg_authid')='` + marker + `'
   AND NOT EXISTS (SELECT FROM pg_auth_members WHERE roleid=r.oid)
   AND (SELECT count(*) FROM pg_auth_members WHERE member=r.oid)=1
   AND EXISTS (SELECT FROM pg_auth_members a JOIN pg_roles o ON o.oid=a.roleid WHERE a.member=r.oid AND o.rolname='` + owner + `' AND NOT a.admin_option AND a.inherit_option AND a.set_option)
  ) THEN RAISE EXCEPTION 'Generation login ownership cannot be verified'; END IF;
 ELSE
  CREATE ROLE "` + login + `" LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '` + verifier + `';
  COMMENT ON ROLE "` + login + `" IS '` + marker + `';
  GRANT "` + owner + `" TO "` + login + `" WITH ADMIN FALSE;
 END IF;
 IF EXISTS (SELECT FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname='` + login + `'
  AND (s.setdatabase=0 OR s.setdatabase<>(SELECT oid FROM pg_database WHERE datname='` + database + `') OR s.setconfig<>ARRAY['role=` + owner + `'])) THEN
  RAISE EXCEPTION 'Generation session configuration drift';
 END IF;
END
$impreza$;
ALTER ROLE "` + login + `" IN DATABASE "` + database + `" SET role TO '` + owner + `';
COMMIT;
`
	return sql, nil
}

func (d *Docker) provisionPostgresGeneration(ctx context.Context, consumer string, c sdkclient.ServiceBindingCredential) (string, error) {
	sql, err := postgresGenerationSQL(consumer, c)
	if err != nil {
		return "", err
	}
	return d.provisionPostgresBindingSQL(ctx, c, sql)
}
