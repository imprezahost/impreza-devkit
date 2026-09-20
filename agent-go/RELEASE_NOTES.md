# Agent 0.6.17

Adds per-application resource metrics and managed MariaDB connection lifecycle:
creation, removal and credential rotation. Metrics contain numeric resource
measurements and container state, not environment values. Their default interval
is 60 seconds. Unknown or stale observations do not mean healthy.

MariaDB operations require an explicit reviewed plan and supported application
layout. Rotation requires a healthy application; database contents are retained
on connection removal. PostgreSQL backup/restore remains a separate capability.

Update explicitly after active operations finish. Configuration, identity and
applications are preserved; no automatic fleet upgrade occurs.
