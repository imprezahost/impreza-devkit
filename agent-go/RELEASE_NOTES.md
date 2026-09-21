# Agent 0.6.18

Adds verified backups and assisted restoration for managed MariaDB InnoDB tables,
and supports reviewed PostgreSQL restoration to an eligible binding on another
host. Restores create a new database; applications are never repointed automatically.

PostgreSQL and MariaDB verification and restore use an operation-specific login
restricted to the new database, without membership of the stable owner. Successful
completion removes the login. Unsupported MariaDB views,
triggers, routines, events and non-transactional tables are refused. Keep schema
changes paused while backing up; filesystem and database snapshots are separate.

Database grants use exact names, and administrator SQL output is excluded from
logs. Existing MariaDB grants are normalized on the next binding deployment.
A host crash can require administrator reconciliation of temporary objects.

Update explicitly after active operations finish. Identity, configuration and
applications are preserved. See [database recovery](https://docs.imprezahost.com/customer-workflows.html#restore).
