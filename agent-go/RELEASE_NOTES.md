# Agent 0.6.15

Requires Docker health checks for PostgreSQL credential rotation and abandonment.
The replacement must report healthy before an unused login can be disabled.
Missing or optional startup policies are refused; review the operation again
with the updated API. A running container without a healthcheck is insufficient.
Health assurance depends on what the application healthcheck actually verifies.

After active deployments finish, update explicitly using the documented update
command. Identity, configuration and applications are preserved. There is no
automatic fleet update. See the [connection guide](https://docs.imprezahost.com/service-bindings.html).
