package executor

import (
	"context"
	"crypto/sha1" //nolint:gosec // mysql_native_password is the engine's verifier shape, never our choice of primitive
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// MariaDB/MySQL binding lifecycle (mysql-service-binding-v2). MariaDB differs
// from PostgreSQL in ways that shape this file:
//   - users are 'login'@'host'; there is no role ownership, no REASSIGN OWNED
//     and no NOLOGIN. A grant-less account can still authenticate, so
//     retirement is REVOKE + DROP USER, never a disable.
//   - there is no SCRAM; the SQL script carries the mysql_native_password
//     verifier hash ("*" + upper(hex(sha1(sha1(password))))), never plaintext,
//     so a failed statement or an administrator-enabled general log cannot
//     expose the credential.
//   - there is no COMMENT ON USER; ownership markers live in a private
//     registry (database `impreza_ownership`, writable only by the
//     administrator channel) so a foreign object with a colliding name is
//     refused, never adopted.

// mysqlBindingVerifier renders the mysql_native_password authentication string
// for IDENTIFIED VIA ... USING. Generated passwords are ASCII hex.
func mysqlBindingVerifier(password string) (string, error) {
	if !bindingRevisionPattern.MatchString(password) {
		return "", errors.New("invalid binding password")
	}
	stage1 := sha1.Sum([]byte(password)) //nolint:gosec // engine-mandated verifier
	stage2 := sha1.Sum(stage1[:])        //nolint:gosec // engine-mandated verifier
	return "*" + strings.ToUpper(hex.EncodeToString(stage2[:])), nil
}

func mysqlGenerationNames(ref sdkclient.ServiceBindingRef) (database, login string) {
	database, _, login = bindingGenerationNames(ref)
	return
}

// One provisioning cycle: guarded adoption checks, dedicated database, one
// per-revision login granted on exactly that database. Idempotent on retry:
// existing marked objects are kept and the final login probe proves the
// credential; a password drifted out of band fails loudly there.
func mysqlGenerationSQL(consumer string, c sdkclient.ServiceBindingCredential) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, login := mysqlGenerationNames(c.ServiceBindingRef)
	if c.Username != login || c.Database != database || !bindingRevisionPattern.MatchString(c.Password) || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid service credential generation")
	}
	verifier, err := mysqlBindingVerifier(c.Password)
	if err != nil {
		return "", err
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	ownerMarker := "impreza-owner:" + identity
	marker := "impreza-generation:" + identity + ":" + c.Revision
	// All substituted values are strict hex/identifier grammars. The marker
	// strings and names can never carry a quote.
	sql := `DELIMITER $$
BEGIN NOT ATOMIC
 IF COALESCE(GET_LOCK('` + ownerMarker + `', 10), 0) <> 1 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation lock unavailable';
 END IF;
END$$
DELIMITER ;
CREATE DATABASE IF NOT EXISTS ` + "`impreza_ownership`" + `;
CREATE TABLE IF NOT EXISTS ` + "`impreza_ownership`.`bindings`" + ` (` + "`name`" + ` VARCHAR(128) NOT NULL PRIMARY KEY, ` + "`marker`" + ` VARCHAR(190) NOT NULL) ENGINE=InnoDB;
DELIMITER $$
BEGIN NOT ATOMIC
 IF EXISTS (SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='` + database + `') AND NOT EXISTS (SELECT 1 FROM ` + "`impreza_ownership`.`bindings`" + ` WHERE ` + "`name`" + `='db:` + database + `' AND ` + "`marker`" + `='` + ownerMarker + `') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation database cannot be adopted';
 END IF;
 IF EXISTS (SELECT 1 FROM ` + "`impreza_ownership`.`bindings`" + ` WHERE ` + "`name`" + `='db:` + database + `' AND ` + "`marker`" + `<>'` + ownerMarker + `') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation database marker mismatch';
 END IF;
 IF EXISTS (SELECT 1 FROM ` + "`mysql`.`user`" + ` WHERE ` + "`User`" + `='` + login + `' AND ` + "`Host`" + `='%') AND NOT EXISTS (SELECT 1 FROM ` + "`impreza_ownership`.`bindings`" + ` WHERE ` + "`name`" + `='user:` + login + `' AND ` + "`marker`" + `='` + marker + `') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation login cannot be adopted';
 END IF;
 IF EXISTS (SELECT 1 FROM ` + "`impreza_ownership`.`bindings`" + ` WHERE ` + "`name`" + `='user:` + login + `' AND ` + "`marker`" + `<>'` + marker + `') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation login marker mismatch';
 END IF;
 IF EXISTS (SELECT 1 FROM information_schema.USER_PRIVILEGES WHERE GRANTEE='''` + login + `''@''%''' AND PRIVILEGE_TYPE<>'USAGE') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation login holds global privileges';
 END IF;
 IF EXISTS (SELECT 1 FROM ` + "`mysql`.`db`" + ` WHERE ` + "`User`" + `='` + login + `' AND (` + "`Host`" + `<>'%' OR ` + "`Db`" + ` NOT IN ('` + database + `','` + strings.ReplaceAll(mysqlExactDatabaseGrant(database), `\`, `\\`) + `') OR ` + "`Grant_priv`" + `<>'N')) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation login grant drift';
 END IF;
 IF EXISTS (SELECT 1 FROM ` + "`mysql`.`tables_priv`" + ` WHERE ` + "`User`" + `='` + login + `') OR EXISTS (SELECT 1 FROM ` + "`mysql`.`procs_priv`" + ` WHERE ` + "`User`" + `='` + login + `') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation login grant drift';
 END IF;
END$$
DELIMITER ;
CREATE DATABASE IF NOT EXISTS ` + "`" + database + "`" + ` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS '` + login + `'@'%' IDENTIFIED VIA mysql_native_password USING '` + verifier + `';
DELIMITER $$
BEGIN NOT ATOMIC
 IF EXISTS (SELECT 1 FROM mysql.db WHERE User='` + login + `' AND Host='%' AND Db='` + database + `') THEN
  REVOKE ALL PRIVILEGES ON ` + "`" + database + "`" + `.* FROM '` + login + `'@'%';
 END IF;
END$$
DELIMITER ;
GRANT ALL PRIVILEGES ON ` + "`" + mysqlExactDatabaseGrant(database) + "`" + `.* TO '` + login + `'@'%';
INSERT INTO ` + "`impreza_ownership`.`bindings`" + ` (` + "`name`" + `, ` + "`marker`" + `) VALUES ('db:` + database + `','` + ownerMarker + `'), ('user:` + login + `','` + marker + `') ON DUPLICATE KEY UPDATE ` + "`marker`" + `=VALUES(` + "`marker`" + `);
`
	return sql, nil
}

// Retirement kills the login's sessions, revokes and DROPS it (MariaDB cannot
// disable an account; a grant-less account still authenticates). The database
// and its data — and the database's ownership marker — are always retained.
// Idempotent: an already-dropped login with no marker is a completed removal.
func mysqlGenerationRetireSQL(consumer string, c sdkclient.ServiceBindingRetirement) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, login := mysqlGenerationNames(c.ServiceBindingRef)
	if c.Username != login || c.Database != database || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid generation retirement identity")
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	ownerMarker := "impreza-owner:" + identity
	marker := "impreza-generation:" + identity + ":" + c.Revision
	sql := `DELIMITER $$
BEGIN NOT ATOMIC
 IF COALESCE(GET_LOCK('` + ownerMarker + `', 10), 0) <> 1 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation lock unavailable';
 END IF;
 IF EXISTS (SELECT 1 FROM ` + "`mysql`.`user`" + ` WHERE ` + "`User`" + `='` + login + `' AND ` + "`Host`" + `='%') AND NOT EXISTS (SELECT 1 FROM ` + "`impreza_ownership`.`bindings`" + ` WHERE ` + "`name`" + `='user:` + login + `' AND ` + "`marker`" + `='` + marker + `') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'Generation retirement login cannot be verified';
 END IF;
END$$
DELIMITER ;
DELIMITER $$
BEGIN NOT ATOMIC
 DECLARE remaining INT DEFAULT 0;
 SELECT COUNT(*) INTO remaining FROM information_schema.PROCESSLIST WHERE ` + "`USER`" + `='` + login + `';
 WHILE remaining > 0 DO
  SET @impreza_kill = CONCAT('KILL ', (SELECT ID FROM information_schema.PROCESSLIST WHERE ` + "`USER`" + `='` + login + `' ORDER BY ID LIMIT 1));
  PREPARE stmt FROM @impreza_kill;
  EXECUTE stmt;
  DEALLOCATE PREPARE stmt;
  SELECT COUNT(*) INTO remaining FROM information_schema.PROCESSLIST WHERE ` + "`USER`" + `='` + login + `';
 END WHILE;
END$$
DELIMITER ;
DELIMITER $$
BEGIN NOT ATOMIC
 IF EXISTS (SELECT 1 FROM ` + "`mysql`.`user`" + ` WHERE ` + "`User`" + `='` + login + `' AND ` + "`Host`" + `='%') THEN
  REVOKE ALL PRIVILEGES, GRANT OPTION FROM '` + login + `'@'%';
  DROP USER '` + login + `'@'%';
 END IF;
END$$
DELIMITER ;
DELETE FROM ` + "`impreza_ownership`.`bindings`" + ` WHERE ` + "`name`" + `='user:` + login + `' AND ` + "`marker`" + `='` + marker + `';
`
	return sql, nil
}

// The administrator channel is the provider container's own root credential:
// read from the container environment through the Docker socket and piped via
// stdin (never a statement, a command line or the job payload), exactly like
// the login verifier below. Command output is discarded: SQL errors can echo
// text. Images that randomize or hash the root credential without exposing
// MARIADB_ROOT_PASSWORD are refused before any statement runs.
func (d *Docker) runMysqlAdminSQL(ctx context.Context, containerID string, sql string) error {
	out, err := d.dockerCmd(ctx, "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", containerID).Output()
	if err != nil {
		return errors.New("provider administrator credential unavailable")
	}
	password := ""
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "MARIADB_ROOT_PASSWORD="); ok && v != "" {
			password = v
			break
		}
	}
	if password == "" {
		return errors.New("provider administrator credential unavailable")
	}
	command := d.dockerCmd(ctx, "exec", "-i", containerID, "sh", "-c", `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mariadb --user=root --batch`, "binding-admin")
	command.Stdin = strings.NewReader(password + "\n" + sql)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("service database administration failed; no credentials or SQL output were returned")
	}
	return nil
}

// Provisioning and retirement share runMysqlAdminSQL as the administrator
// channel: no credential ever enters a statement, a command line or the job
// payload, and command output is discarded because SQL errors can echo text.
func (d *Docker) provisionMysqlGeneration(ctx context.Context, consumer string, c sdkclient.ServiceBindingCredential) (string, error) {
	sql, err := mysqlGenerationSQL(consumer, c)
	if err != nil {
		return "", err
	}
	return d.provisionMysqlBindingSQL(ctx, c, sql)
}

func (d *Docker) provisionMysqlBindingSQL(ctx context.Context, c sdkclient.ServiceBindingCredential, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	containers, networks, err := d.mysqlProviderState(ctx, c.ProviderDeploymentID)
	if err != nil {
		return "", err
	}
	address, err := bindingProviderAddress(containers[0], networks[0])
	if err != nil {
		return "", err
	}
	if err = d.runMysqlAdminSQL(ctx, containers[0].ID, sql); err != nil {
		return "", err
	}
	// Explicit negative control first: a successful query alone never proves
	// password authentication. The password rides MYSQL_PWD via stdin, never
	// the command line (process lists are visible inside the provider).
	login := func(password string) error {
		verify := d.dockerCmd(ctx, "exec", "-i", containers[0].ID, "sh", "-c", `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mariadb --host="$1" --user="$2" --database="$3" --batch --skip-column-names -e "SELECT 1"`, "binding-check", address, c.Username, c.Database)
		verify.Stdin = strings.NewReader(password + "\n")
		verify.Stdout = io.Discard
		verify.Stderr = io.Discard
		return verify.Run()
	}
	wrong := "0" + c.Password[1:]
	if c.Password[0] == '0' {
		wrong = "1" + c.Password[1:]
	}
	if login(wrong) == nil {
		return "", errors.New("service provider accepted an invalid password; authentication cannot be verified")
	}
	if err = login(c.Password); err != nil {
		return "", errors.New("service database login could not be verified")
	}
	u := url.URL{Scheme: "mysql", User: url.UserPassword(c.Username, c.Password), Host: "mariadb_" + c.ProviderDeploymentID + ":3306", Path: "/" + c.Database}
	return u.String(), nil
}

func (d *Docker) retireMysqlGeneration(ctx context.Context, consumer string, c sdkclient.ServiceBindingRetirement) error {
	sql, err := mysqlGenerationRetireSQL(consumer, c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	containers, _, err := d.mysqlProviderState(ctx, c.ProviderDeploymentID)
	if err != nil {
		return err
	}
	return d.runMysqlAdminSQL(ctx, containers[0].ID, sql)
}

// Same identity discipline as the PostgreSQL provider check, against the
// catalog MariaDB application: one running container of the provider's own
// compose project, on its own default bridge network.
func (d *Docker) mysqlProviderState(ctx context.Context, provider string) ([]bindingProviderContainer, []bindingProviderNetwork, error) {
	var containers []bindingProviderContainer
	var networks []bindingProviderNetwork
	raw, err := limitedRuntimeOutput(d.dockerCmd(ctx, "inspect", "mariadb_"+provider), 1024*1024)
	if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, &containers) != nil {
		return nil, nil, errors.New("service provider container is unavailable")
	}
	raw, err = limitedRuntimeOutput(d.dockerCmd(ctx, "network", "inspect", provider+"_default"), 1024*1024)
	if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, &networks) != nil {
		return nil, nil, errors.New("service provider network is unavailable")
	}
	if !bindingDeploymentPattern.MatchString(provider) || len(containers) != 1 || len(networks) != 1 {
		return nil, nil, errors.New("service provider identity unavailable")
	}
	cn, n := containers[0], networks[0]
	if !bindingRevisionPattern.MatchString(cn.ID) || !bindingRevisionPattern.MatchString(n.ID) || !cn.State.Running || cn.Config.Labels["com.docker.compose.project"] != provider || cn.Config.Labels["com.docker.compose.service"] != "mariadb" || n.Name != provider+"_default" || n.Driver != "bridge" || n.Labels["com.docker.compose.project"] != provider || n.Labels["com.docker.compose.network"] != "default" {
		return nil, nil, errors.New("service provider network ownership cannot be verified")
	}
	if _, ok := n.Containers[cn.ID]; !ok {
		return nil, nil, errors.New("service provider is absent from its owned network")
	}
	return containers, networks, nil
}
