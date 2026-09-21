package executor

import (
	"context"
	"errors"
	"net/url"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// MariaDB engine's backup database stage (mysql-service-binding-backup-v1).
// Same contract as the PostgreSQL stage in backup_database.go: the JIT
// credential only ever reaches the job's private .env, and the mandatory
// verification lands in a scratch database created and dropped through the
// provider's administrator channel. The engine differences mirror the
// binding lifecycle (service_binding_mysql.go): ownership markers live in
// the private impreza_ownership registry instead of catalog comments, and
// the administrator channel is the provider container's own root
// credential, so the credential's AdminUser never enters a statement.

// mysqlBackupURL renders a database URL for the supplied scoped credential.
// The dump receives the serving credential; verification receives a job login.
func mysqlBackupURL(c sdkclient.ServiceBindingCredential, database string) (string, error) {
	if !bindingRevisionPattern.MatchString(c.Password) {
		return "", errors.New("invalid backup database credential shape")
	}
	u := url.URL{Scheme: "mysql", User: url.UserPassword(c.Username, c.Password), Host: "mariadb_" + c.ProviderDeploymentID + ":3306", Path: "/" + database}
	return u.String(), nil
}

// The scratch database proves the dump restores before the backup is
// reported. It is created and dropped only through the administrator
// channel; the binding login never receives any grant on it. The registry
// marker is what lets the drop path distinguish our scratch from anything
// else — and what refuses to adopt a foreign database with a colliding
// name.
func mysqlBackupScratchSQL(consumer string, c sdkclient.ServiceBindingCredential, scratch string) (string, error) {
	return mysqlDatabaseJobSQL(consumer, c, scratch, true, false)
}

func mysqlBackupScratchDropSQL(consumer string, c sdkclient.ServiceBindingCredential, scratch string) (string, error) {
	return mysqlDatabaseJobSQL(consumer, c, scratch, false, false)
}

// Verified provider channel: the same container/network identity check as
// the lifecycle, then the statement runs through the administrator channel.
func (d *Docker) runMysqlBackupSQL(ctx context.Context, c sdkclient.ServiceBindingCredential, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	containers, _, err := d.mysqlProviderState(ctx, c.ProviderDeploymentID)
	if err != nil {
		return err
	}
	return d.runMysqlAdminSQL(ctx, containers[0].ID, sql)
}

func (d *Docker) createMysqlBackupScratch(ctx context.Context, spec *sdkclient.BackupDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := mysqlBackupScratchSQL(spec.ConsumerDeploymentID, c, spec.VerifyDatabase)
	if err != nil {
		return err
	}
	return d.runMysqlBackupSQL(ctx, c, sql)
}

func (d *Docker) dropMysqlBackupScratch(ctx context.Context, spec *sdkclient.BackupDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := mysqlBackupScratchDropSQL(spec.ConsumerDeploymentID, c, spec.VerifyDatabase)
	if err != nil {
		return err
	}
	return d.runMysqlBackupSQL(ctx, c, sql)
}
