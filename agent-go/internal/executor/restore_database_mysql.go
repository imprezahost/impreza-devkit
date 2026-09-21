package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// MariaDB engine's restore database stage (mysql-service-binding-restore-v1).
// Mirrors restore_database.go: the reviewed dump lands in a NEW database the
// agent creates through the provider's administrator channel; any failure
// drops the new database, and the serving database is never named as a
// target. Markers live in the private impreza_ownership registry.

// mysqlRestoreURL renders the operation-specific URL used by the restore stack.
func mysqlRestoreURL(c sdkclient.ServiceBindingCredential, database string) (string, error) {
	if !bindingRevisionPattern.MatchString(c.Password) {
		return "", errors.New("invalid restore database credential shape")
	}
	u := url.URL{Scheme: "mysql", User: url.UserPassword(c.Username, c.Password), Host: "mariadb_" + c.ProviderDeploymentID + ":3306", Path: "/" + database}
	return u.String(), nil
}

// The new database carries the restore marker in the registry — that is
// what the drop path checks, and what lets a later deploy's ownership
// guards recognize a database the product itself created. A name that
// already exists is refused, never adopted.
func mysqlRestoreDatabaseSQL(consumer string, c sdkclient.ServiceBindingCredential, name string) (string, error) {
	return mysqlDatabaseJobSQL(consumer, c, name, true, false)
}

func mysqlRestoreDropSQL(consumer string, c sdkclient.ServiceBindingCredential, name string) (string, error) {
	return mysqlDatabaseJobSQL(consumer, c, name, false, false)
}

// runMysqlRestoreSQL is the administrator channel with bounded output: the
// restore's table count must come from the provider itself, so unlike the
// lifecycle channel this variant captures stdout (never stderr — SQL errors
// can echo text) through the same limited reader the runtime uses.
func (d *Docker) runMysqlRestoreSQL(ctx context.Context, c sdkclient.ServiceBindingCredential, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	containers, _, err := d.mysqlProviderState(ctx, c.ProviderDeploymentID)
	if err != nil {
		return "", err
	}
	containerID := containers[0].ID
	out, err := d.dockerCmd(ctx, "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", containerID).Output()
	if err != nil {
		return "", errors.New("provider administrator credential unavailable")
	}
	password := ""
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "MARIADB_ROOT_PASSWORD="); ok && v != "" {
			password = v
			break
		}
	}
	if password == "" {
		return "", errors.New("provider administrator credential unavailable")
	}
	command := d.dockerCmd(ctx, "exec", "-i", containerID, "sh", "-c", `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mariadb --user=root --batch --skip-column-names`, "restore-admin")
	command.Stdin = strings.NewReader(password + "\n" + sql)
	command.Stderr = io.Discard
	raw, err := limitedRuntimeOutput(command, 64*1024)
	if err != nil {
		return "", errors.New("restore database statement could not be verified")
	}
	return string(raw), nil
}

func (d *Docker) createMysqlRestoreDatabase(ctx context.Context, spec *sdkclient.RestoreDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := mysqlRestoreDatabaseSQL(spec.ConsumerDeploymentID, c, spec.RestoreDatabase)
	if err != nil {
		return err
	}
	_, err = d.runMysqlRestoreSQL(ctx, c, sql)
	return err
}

func (d *Docker) dropMysqlRestoreDatabase(ctx context.Context, spec *sdkclient.RestoreDatabaseSpec, c sdkclient.ServiceBindingCredential) error {
	sql, err := mysqlRestoreDropSQL(spec.ConsumerDeploymentID, c, spec.RestoreDatabase)
	if err != nil {
		return err
	}
	_, err = d.runMysqlRestoreSQL(ctx, c, sql)
	return err
}

// The outcome's table count comes from the provider itself, never from the
// job container's self-report.
func (d *Docker) countMysqlRestoreTables(ctx context.Context, spec *sdkclient.RestoreDatabaseSpec, c sdkclient.ServiceBindingCredential) (int, error) {
	if err := validateServiceBindingRef(spec.ConsumerDeploymentID, c.ServiceBindingRef); err != nil {
		return 0, err
	}
	if !restoreDatabasePattern.MatchString(spec.RestoreDatabase) {
		return 0, errors.New("invalid restore database identity")
	}
	out, err := d.runMysqlRestoreSQL(ctx, c,
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='"+spec.RestoreDatabase+"';\n")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("restored table count is not a number: %q", strings.TrimSpace(out))
	}
	return n, nil
}
