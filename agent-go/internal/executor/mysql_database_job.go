package executor

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// A dump is executable SQL. Verification and restoration must never use the
// serving login: even a reviewed archive cannot be trusted with the live DB.
// Derive a separate secret for this one operation; it is delivered only to its
// private job environment, and the account is removed before success is returned.
func mysqlDatabaseJobCredential(c sdkclient.ServiceBindingCredential, name string) (sdkclient.ServiceBindingCredential, error) {
	if !backupScratchPattern.MatchString(name) && !restoreDatabasePattern.MatchString(name) {
		return c, errors.New("invalid database job identity")
	}
	if !bindingRevisionPattern.MatchString(c.Password) {
		return c, errors.New("invalid database job credential")
	}
	mac := hmac.New(sha256.New, []byte(c.Password))
	mac.Write([]byte("impreza-mysql-job-v1:" + c.BindingID + ":" + c.ProviderDeploymentID + ":" + name))
	c.Password = hex.EncodeToString(mac.Sum(nil))
	prefix := "ijv_"
	if restoreDatabasePattern.MatchString(name) {
		prefix = "ijr_"
	}
	c.Username = prefix + name[len(name)-16:]
	c.Database = name
	return c, nil
}

// Database-level GRANT treats underscores as wildcards, even in backticks.
func mysqlExactDatabaseGrant(name string) string { return strings.ReplaceAll(name, "_", `\_`) }

func mysqlDatabaseJobSQL(consumer string, c sdkclient.ServiceBindingCredential, name string, create, keep bool) (string, error) {
	if err := validateServiceBindingRef(consumer, c.ServiceBindingRef); err != nil {
		return "", err
	}
	database, login := mysqlGenerationNames(c.ServiceBindingRef)
	if c.Database != database || c.Username != login || !bindingAdminPattern.MatchString(c.AdminUser) {
		return "", errors.New("invalid database job binding identity")
	}
	job, err := mysqlDatabaseJobCredential(c, name)
	if err != nil {
		return "", err
	}
	verifier, err := mysqlBindingVerifier(job.Password)
	if err != nil {
		return "", err
	}
	identity := c.BindingID + ":" + c.ProviderDeploymentID + ":" + consumer
	marker := "impreza-backup-verify:" + identity
	if restoreDatabasePattern.MatchString(name) {
		marker = "impreza-restore:" + identity
	}
	replace := strings.NewReplacer("@OWNER@", "impreza-owner:"+identity, "@DB@", database,
		"@NAME@", name, "@LOGIN@", job.Username, "@MARKER@", marker,
		"@GRANT@", mysqlExactDatabaseGrant(name), "@VERIFIER@", verifier)
	// All substitutions are validated identifiers, hex, or fixed marker strings.
	sql := `DELIMITER $$
BEGIN NOT ATOMIC
 IF COALESCE(GET_LOCK('@OWNER@',10),0) <> 1 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Database job lock unavailable';
 END IF;
END$$
DELIMITER ;
`
	if create {
		// Validate collisions BEFORE entering the cleanup handler. Failed creation
		// rolls back only objects this invocation just created, never an existing DB.
		sql += `DELIMITER $$
BEGIN NOT ATOMIC
 IF NOT EXISTS (SELECT 1 FROM impreza_ownership.bindings WHERE name='db:@DB@' AND marker='@OWNER@') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Database job binding ownership cannot be verified';
 END IF;
 IF EXISTS (SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='@NAME@')
  OR EXISTS (SELECT 1 FROM mysql.user WHERE User='@LOGIN@')
  OR EXISTS (SELECT 1 FROM impreza_ownership.bindings WHERE name IN ('db:@NAME@','user:@LOGIN@')) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Database job name is already taken';
 END IF;
 BEGIN
  DECLARE EXIT HANDLER FOR SQLEXCEPTION
  BEGIN
   DROP USER IF EXISTS '@LOGIN@'@'%';
   DROP DATABASE IF EXISTS ` + "`@NAME@`" + `;
   DELETE FROM impreza_ownership.bindings WHERE name IN ('db:@NAME@','user:@LOGIN@') AND marker='@MARKER@';
   RESIGNAL;
  END;
  CREATE DATABASE ` + "`@NAME@`" + ` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
  CREATE USER '@LOGIN@'@'%' IDENTIFIED VIA mysql_native_password USING '@VERIFIER@';
  GRANT SELECT,INSERT,UPDATE,DELETE,CREATE,DROP,INDEX,ALTER,REFERENCES,CREATE VIEW,SHOW VIEW,TRIGGER
   ON ` + "`@GRANT@`" + `.* TO '@LOGIN@'@'%';
  INSERT INTO impreza_ownership.bindings (name,marker) VALUES ('db:@NAME@','@MARKER@'),('user:@LOGIN@','@MARKER@');
 END;
END$$
DELIMITER ;
`
	} else {
		sql += `DELIMITER $$
BEGIN NOT ATOMIC
 IF EXISTS (SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='@NAME@') AND NOT EXISTS
  (SELECT 1 FROM impreza_ownership.bindings WHERE name='db:@NAME@' AND marker='@MARKER@') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Database job ownership cannot be verified';
 END IF;
 IF EXISTS (SELECT 1 FROM mysql.user WHERE User='@LOGIN@') AND NOT EXISTS
  (SELECT 1 FROM impreza_ownership.bindings WHERE name='user:@LOGIN@' AND marker='@MARKER@') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Database job login ownership cannot be verified';
 END IF;
 IF EXISTS (SELECT 1 FROM mysql.user WHERE User='@LOGIN@' AND Host<>'%') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Database job login host mismatch';
 END IF;
END$$
DELIMITER ;
DROP USER IF EXISTS '@LOGIN@'@'%';
DELIMITER $$
BEGIN NOT ATOMIC
 WHILE EXISTS (SELECT 1 FROM information_schema.PROCESSLIST WHERE USER='@LOGIN@') DO
  SET @impreza_kill = CONCAT('KILL ', (SELECT ID FROM information_schema.PROCESSLIST WHERE USER='@LOGIN@' ORDER BY ID LIMIT 1));
  PREPARE stmt FROM @impreza_kill;
  EXECUTE stmt;
  DEALLOCATE PREPARE stmt;
 END WHILE;
END$$
DELIMITER ;
DELETE FROM impreza_ownership.bindings WHERE name='user:@LOGIN@' AND marker='@MARKER@';
`
		if !keep {
			sql += "DROP DATABASE IF EXISTS `@NAME@`;\nDELETE FROM impreza_ownership.bindings WHERE name='db:@NAME@' AND marker='@MARKER@';\n"
		}
	}
	return replace.Replace(sql), nil
}
