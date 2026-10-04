# Coraza Caddy connector (modified copy)

Upstream: github.com/corazawaf/coraza-caddy/v2 v2.6.1 (Apache-2.0).
Coraza v3.7.0 / embedded CRS v4.25.0, unchanged versions.

The local replacement adds only a deployment/mode label and a callback after
transaction logging that extracts static CRS rule IDs. It never copies matched
values, URI, headers, body, addresses or rule messages into telemetry. The native
error callback and debug logger are silent even if a Caddy logger is enabled.
An enforced rule writes its HTTP status directly and completes the response;
returning a Caddy HandlerError would log the full request at DEBUG.
Request/response inspection retains the upstream logic. Interruption completion
is adapted to avoid Caddy request logs, and a permanently disabled debug logger
does not retain fields. Upstream tests remain, with these privacy expectations
updated. The outer application pins this replacement.
