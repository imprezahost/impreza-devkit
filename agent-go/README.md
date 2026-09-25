# impreza-agent

> Managed-server daemon for the Impreza Platform. Runs on every server
> the platform manages — Impreza-provisioned VPS, dedicated, or
> bring-your-own. Connects to the control plane via HTTP long-poll,
> executes commands, and reports outcomes.

## Status

Commands:

- `bootstrap` — exchange a one-time token for permanent credentials.
- `run` — long-poll loop + heartbeat.
- `doctor` — diagnose config / network / credential issues.
- `version` — print build version.

The Docker executor deploys and manages applications. Unsupported commands fail
explicitly. Existing installations update only at the customer’s request.

## Install

### Bring-your-own server (curl | sh)

```bash
curl -fsSL https://raw.githubusercontent.com/imprezahost/agent-public/main/install.sh | \
  IMPREZA_BOOTSTRAP=bst_xxxxxxxxxxxxxxxx sh
```

The installer downloads the matching binary for the host architecture,
installs the systemd unit, and runs `bootstrap` automatically.

### Impreza-provisioned VPS

Provisioning installs the agent automatically (via the existing
PostProvisionRunner path) when the chosen plan has `agent_required:
true`. No manual step.

### Manual

```bash
# Linux x86_64
curl -fsSL -o impreza-agent \
  https://raw.githubusercontent.com/imprezahost/agent-public/main/releases/stable/latest/impreza-agent-linux-amd64
chmod +x impreza-agent
sudo install -m 0755 impreza-agent /usr/local/bin/

sudo impreza-agent bootstrap --token bst_xxxxxxxxxxxxxxxx
sudo systemctl enable --now impreza-agent
```

## Configuration

After `bootstrap` the agent writes `/etc/impreza-agent/config.toml`:

```toml
agent_id        = "agt_xxxxxxxxxxxx"
agent_secret    = "agts_xxxxxxxxxxxxxxxxxxxxxxxxxxxx"  # 0600
control_plane_url = "https://api.imprezahost.com"

use_tor = false
proxy   = ""

# How long to back off after a poll failure. The poll itself blocks
# up to ~55s server-side; this only applies on transport errors.
backoff_min_seconds = 1
backoff_max_seconds = 60

# Heartbeat cadence. Server marks the agent offline after 3 misses.
heartbeat_seconds = 30
```

Override path with `--config /path/to/config.toml`.

## Commands

```
impreza-agent bootstrap --token bst_xxx [--control-plane URL] [--config PATH] [--tor]
impreza-agent run [--config PATH]
impreza-agent doctor [--config PATH]
impreza-agent --version
```

## License

Proprietary — see [`../LICENSE`](../LICENSE).


## Agent updates

Existing installations can check for a new agent without registering again:

```sh
curl -fsSL https://raw.githubusercontent.com/imprezahost/agent-public/main/update.sh | sudo sh -s -- --check
```

After all deployment operations have finished, replace `--check` with `--apply`.
The updater validates the release checksum and version, replaces the binary
atomically and restarts only the agent. It restores the previous executable
if startup fails. Configuration, identity and application containers are kept.
Confirm the new version after the next heartbeat. Stable metadata and binaries
are distributed from agent-public; this command also works with older agents
that do not implement an upgrade command.

## Git revision integrity

Agents 0.6.1 and later honor the full Git commit supplied by a push webhook or manifest build context. If the branch has advanced, the agent fetches and checks out the requested commit. Invalid or unavailable commits fail before container replacement; the current application remains running. When no commit is supplied, deployment follows the selected branch. This does not pin external image tags, dependencies or database state.

## Unsupported commands

Agents 0.6.2 and later reject unsupported command kinds with status failed and an explicit diagnostic, instead of reporting simulated success. No operation is performed and the polling loop continues with subsequent commands. A failed command report does not itself mean the running application is unhealthy.

Agent 0.6.22 adds `agent-upgrade-v1`. An explicit customer request from the portal, API or MCP stages the updater bundled with the agent; only after the control plane acknowledges the job does a separate systemd unit run it. The unit requires a signed channel manifest and the exact version requested, then the next heartbeat verifies the result. Hosts without the prerequisites do not advertise the capability. Agents before 0.6.22 must be updated once with the manual update command above.


## Required healthy startup

Agent 0.6.3 supports opt-in required healthy startup. Generated Node deployments can set `require_healthy_start: true` with an explicit `healthcheck_path` and `startup_timeout_seconds` from 30 to 600 (default 60). The first deployment fails if it does not become healthy; named volumes are preserved. Retained releases keep their own startup policy for automatic recovery and manual rollback. Older agents must be updated explicitly before using this option. See [deployment settings](https://docs.imprezahost.com/tutorials/agent-apps-panels.html#required-startup).


## Runtime health observations

Agent 0.6.4 adds bounded Docker runtime observations to heartbeats. The portal, REST API and MCP show current container state separately from the latest deployment operation, with timestamps and explicit unknown/stale/offline states. Running containers without a healthcheck are not declared healthy. This does not probe external URLs or automatically repair applications. Existing servers update only at the customer's request. See [runtime health](https://docs.imprezahost.com/runtime-health.html).


## Deployment cancellation

Agent 0.6.5 supports deployment cancellation at preparation checkpoints. Queued jobs can be cancelled immediately. During source preparation, image pull or build, cancellation is requested first and confirmed only after the current step finishes and configuration is restored. Existing app containers are not replaced. Replacement and recovery cannot be cancelled. Legacy builds wait for the current step; controlled builds are described below. Update the agent explicitly before the next deploy; an interrupted agent requires operation reconciliation before retry. See [deployment cancellation](https://docs.imprezahost.com/deployment-cancellation.html).

## Deployment progress and persistent results

Agent 0.6.6+ reports deployment steps and persists final results before delivery. After restart, a saved receipt is resent without repeating the deployment. Agent 0.6.7+ can reconcile an interrupted deploy without a final result only from a durable unstarted or completed preparation checkpoint, after verifying the exact operation and the server preparation phase. It verifies unchanged container IDs, restores and verifies previous compose.yaml, .env and startup.json, and records failed or a previously requested cancellation without repeating deployment. recovery=reconciling means verification/restoration is underway; wait for the terminal result before an explicit retry. An external operation without a verified successful completion receipt, an uncertain or authorized replacement, container drift, missing/legacy state, recursive data ownership changes and onion provisioning require support reconciliation (recovery=required). No process absence or timeout proves a build finished. It does not resume builds, replace containers, restore data or promise runtime health. The private journal must be preserved; older operations do not gain checkpoints retroactively. Preserve the private agent state directory; do not remove its operation record to bypass the gate. This does not resume or kill an interrupted Docker build. Update existing agents explicitly before the next deploy. See [deployment progress](https://docs.imprezahost.com/deployment-progress.html).


## Supervised preparation

These legacy worker rules remain in effect unless the administrator enables agent 0.6.12+ [controlled builds](https://docs.imprezahost.com/deployment-cancellation.html#controlled-builds), which add verified executor stop and recovery for new builds.

Agent 0.6.8+: supported Linux/systemd deploys run image pull and build in a separate supervised process. If the agent restarts, it waits for the exact worker receipt without repeating that work. Only a durable successful receipt allows the existing preparation reconciliation: revalidate operation/phase and unchanged containers, restore previous configuration, and close as failed or confirm a previously requested cancellation. recovery=reconciling can include waiting for the original worker. Missing/invalid receipts, worker failure or timeout, host reboot before a receipt, legacy unsupervised work, replacement uncertainty, data ownership and onion preparation still require review. A process or service disappearing is never proof of completion. The customer must wait for the final result before retrying; no automatic deploy retry, immediate build termination, data rollback or runtime-health guarantee is added.


## Supervised replacement

Agent 0.6.9+: new supported Linux/systemd deploys keep the authorized container replacement, startup checks, lifecycle hooks, routes and normal startup recovery in one supervised worker. If the agent restarts, recovery=reconciling with step=reconciling_replacement waits for that original worker. Its verified durable final receipt is delivered without repeating containers or hooks, including a failed deployment whose previous release was restored. Missing or invalid receipts, worker loss or timeout, host reboot before completion, legacy unsupervised operations, data ownership changes and onion provisioning still require support; keep the private journal and do not retry to unblock the queue. This does not add automatic deployment retries, database rollback or zero-downtime traffic switching. Update the agent explicitly before the next deploy.

## Controlled builds

Agent 0.6.12 adds an optional executor for builds on Ubuntu 24.04 amd64 with
systemd, AppArmor, local Docker, Buildx and Compose supporting `--builder`.
Update explicitly after active operations finish. A server administrator then runs:

```sh
sudo impreza-agent builder prepare
sudo impreza-agent builder status
```

Preparation downloads a checksum-pinned executor archive and enables this mode
only for future builds. `builder disable` disables future use; it preserves active
operation recovery state. It does not install host packages, change host sysctls,
rewrite credentials or update other servers.

Cancel through the portal, REST API or MCP during preparation. The agent verifies
the command and its owned executor, stops that executor, restores configuration
and only then confirms cancellation. Legacy builds still wait for their current
step. Image pulls, replacement and recovery are not made forcibly cancellable.

After worker loss or host reboot, verified owned resources can be cleaned without
replaying a build; missing or inconsistent identity receipts require support.
Keep the private journal and wait for the final result before retrying. Existing
application containers and named data volumes are not replaced by cancellation.

The rootless executor has CPU, memory and process limits and disposable cache.
Its scoped AppArmor profile and required seccomp/system-path exceptions do not
provide a sandbox for hostile project code or an outbound network restriction.
Build only trusted projects; build credentials can be read by that project's build.
See [controlled builds](https://docs.imprezahost.com/deployment-cancellation.html#controlled-builds).


## PostgreSQL credential rotation

Reviewed rotation and abandonment require a mandatory healthy-startup policy.
Every running container must provide a Docker healthcheck and report `healthy`
before the unused login can be disabled. A running container without a healthcheck
is insufficient. The API preserves a configured timeout (30–600 seconds), or uses
60 seconds. The required policy remains enabled after completion. Use checks
that test the application's actual readiness, including database access as needed.
Failed checks preserve the credentials for a reviewed retry and recover a verified
previous release when available. See [PostgreSQL connections](https://docs.imprezahost.com/service-bindings.html#rotation).

## Reviewed routing and data workflows

Agent 0.6.16 supports reviewed hostname switches between applications on the same
server, password-protected HTTPS previews, PostgreSQL backups with a restore check,
and assisted restore into a new database. A traffic switch keeps the source running.
If routing recovery cannot be verified, preserve the journal and request support
before attempting another change. Restore does not cut over the application or
provide point-in-time recovery. Update an existing agent explicitly after active
operations finish; identity, configuration and applications are preserved.

The IPv4 egress baseline covers forwarded traffic entering standard Docker bridges.
It blocks outbound mail and private/metadata destinations and limits new connections.
The IPv4 baseline does not cover host networking, custom bridge names or all forms of abuse; IPv6 coverage is described below.
Installation failure is reported and does not stop the agent; this is not a complete
network sandbox. See the deployment-safety and service-bindings guides for limits.

## Runtime isolation and source review (0.6.20)

Agent 0.6.20 adds IPv6 filtering for forwarded traffic entering standard
Docker bridges. Host networking, alternative drivers and host INPUT traffic
remain outside this baseline; it is not a complete network sandbox.

The `tor-egress-v1` capability enables the custom-deployment `tor_egress` option
in the portal/API/MCP. It requires Docker Engine 28+ with isolated gateway
support. Applications receive SOCKS5h proxy settings and an internal network;
only their per-app Tor sidecar has an uplink. Direct external DNS and IPv4/IPv6
runtime connections are refused. Source fetching and builds are outside this
policy. Use SOCKS-aware libraries; this is not transparent UDP forwarding.
Catalog installs, imported manifests and external service bindings are not
supported by the option. See the [Tor guide](https://docs.imprezahost.com/onion-services.html#runtime-egress)
for release availability and customer verification.

Source builds attach a bounded advisory `source_scan`, including incomplete and
unavailable-database states. The scanner reads only the fetched build context,
never the application's persisted data or injected secrets. It does not execute
source code or upload file contents or dependency inventories. The scanner
matches explicit versions and OSV intervals for npm, Packagist and Go using
ecosystem-specific ordering. It consumes `advisories.signed.json` in the private
agent state directory, signed with Ed25519 by the configured trusted publisher.
There is no unsigned fallback. The agent refreshes at startup and every six
hours; updates use a credential-free HTTPS client and honor the agent's Tor/SOCKS
configuration without direct fallback. Failed downloads/signatures retain the
verified cache. Expired caches are marked stale; missing trust/data is unavailable.
Explicit operator commands are `impreza-agent advisories status` and
`impreza-agent advisories update`. Neither command updates the agent binary.

The central `tools/advisory` collector selects GitHub-reviewed and official Go
records from OSV exports, removes withdrawn records, preserves source attribution
and reports unsupported data. Collection and signing are separate commands; the
collector never receives a signing key. The agent embeds the production public key. Operator testing
can explicitly set `IMPREZA_ADVISORY_PUBLIC_KEY` (base64 Ed25519 public key) and
`IMPREZA_ADVISORY_URL` (HTTPS, no query/credentials/redirects) in the root-owned
service environment. Applications cannot configure either setting. Never use a
test signing key for a release or put a private signing key on an agent.

A missing or partial database cannot
produce a clean dependency assessment. `go.sum` can include unused versions;
matching does not prove vulnerable code is reachable. See
[source review limits](https://docs.imprezahost.com/deployment-safety.html#source-scan).

The advisory status includes `trusted_key_sha256`. The prepared production trust
fingerprint is `29a64f4eff2331ef46891bfe6537f939a1c051f4cba28a79476bbefa5c64d6b3`.
A public fingerprint is not a signing credential.

## Private previews and onion identity retention (0.6.20)

`onion-private-preview-v1` allows the control plane to supply reviewer keys as
part of the first deploy. The service is never intentionally published without
its initial authorization policy. Repeated deploys preserve later revocations;
the agent refuses to silently convert a private preview to public discovery.
Client authorization files use `<56-character-service-id>:descriptor:x25519:<private-key>`.

HTTP and HTTPS onion Git sources use a temporary Tor client with remote DNS.
Redirects and direct network fallback are disabled. This protects the repository
request path; image pulls, dependency downloads and build steps are separate.

The Go CLI supports confirmed purge of retained onion identity copies. Active
identities are protected. Completion reports copies removed from the managed
host; it does not erase exports, backups or previously disclosed key material.
