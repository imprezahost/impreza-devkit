# impreza-agent

> Managed-server daemon for the Impreza Platform. Runs on every server
> the platform manages — Impreza-provisioned VPS, dedicated, or
> bring-your-own. Connects to the control plane via HTTP long-poll,
> executes commands, and reports outcomes.

## Status

**MVP (Phase 9.1).** Surface implemented:

- `bootstrap` — exchange a one-time token for permanent credentials.
- `run` — long-poll loop + heartbeat.
- `doctor` — diagnose config / network / credential issues.
- `version` — print build version.

**Executor:** `echo` only — every command returns `success` immediately.
Real executors (Docker, systemd, Caddy) land in Phase 9.2+.

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

Agents 0.6.2 and later reject unsupported command kinds with status failed and an explicit diagnostic, instead of reporting simulated success. No operation is performed and the polling loop continues with subsequent commands. Queued agent_upgrade remains unsupported; use the customer-initiated update command. A failed command report does not itself mean the running application is unhealthy.


## Required healthy startup

Agent 0.6.3 supports opt-in required healthy startup. Generated Node deployments can set `require_healthy_start: true` with an explicit `healthcheck_path` and `startup_timeout_seconds` from 30 to 600 (default 60). The first deployment fails if it does not become healthy; named volumes are preserved. Retained releases keep their own startup policy for automatic recovery and manual rollback. Older agents must be updated explicitly before using this option. See [deployment settings](https://docs.imprezahost.com/tutorials/agent-apps-panels.html#required-startup).


## Runtime health observations

Agent 0.6.4 adds bounded Docker runtime observations to heartbeats. The portal, REST API and MCP show current container state separately from the latest deployment operation, with timestamps and explicit unknown/stale/offline states. Running containers without a healthcheck are not declared healthy. This does not probe external URLs or automatically repair applications. Existing servers update only at the customer's request. See [runtime health](https://docs.imprezahost.com/runtime-health.html).


## Deployment cancellation

Agent 0.6.5 supports deployment cancellation at preparation checkpoints. Queued jobs can be cancelled immediately. During source preparation, image pull or build, cancellation is requested first and confirmed only after the current step finishes and configuration is restored. Existing app containers are not replaced. Replacement and recovery cannot be cancelled. A running build is not force-killed. Update the agent explicitly before the next deploy; an interrupted agent requires operation reconciliation before retry. See [deployment cancellation](https://docs.imprezahost.com/deployment-cancellation.html).

## Deployment progress and persistent results

Agent 0.6.6+ reports deployment steps and persists final results before delivery. After restart, a saved receipt is resent without repeating the deployment. Agent 0.6.7+ can reconcile an interrupted deploy without a final result only from a durable unstarted or completed preparation checkpoint, after verifying the exact operation and the server preparation phase. It verifies unchanged container IDs, restores and verifies previous compose.yaml, .env and startup.json, and records failed or a previously requested cancellation without repeating deployment. recovery=reconciling means verification/restoration is underway; wait for the terminal result before an explicit retry. An external operation without a verified successful completion receipt, an uncertain or authorized replacement, container drift, missing/legacy state, recursive data ownership changes and onion provisioning require support reconciliation (recovery=required). No process absence or timeout proves a build finished. It does not resume builds, replace containers, restore data or promise runtime health. The private journal must be preserved; older operations do not gain checkpoints retroactively. Preserve the private agent state directory; do not remove its operation record to bypass the gate. This does not resume or kill an interrupted Docker build. Update existing agents explicitly before the next deploy. See [deployment progress](https://docs.imprezahost.com/deployment-progress.html).


## Supervised preparation

Agent 0.6.8+: supported Linux/systemd deploys run image pull and build in a separate supervised process. If the agent restarts, it waits for the exact worker receipt without repeating that work. Only a durable successful receipt allows the existing preparation reconciliation: revalidate operation/phase and unchanged containers, restore previous configuration, and close as failed or confirm a previously requested cancellation. recovery=reconciling can include waiting for the original worker. Missing/invalid receipts, worker failure or timeout, host reboot before a receipt, legacy unsupervised work, replacement uncertainty, data ownership and onion preparation still require review. A process or service disappearing is never proof of completion. The customer must wait for the final result before retrying; no automatic deploy retry, immediate build termination, data rollback or runtime-health guarantee is added.


## Supervised replacement

Agent 0.6.9+: new supported Linux/systemd deploys keep the authorized container replacement, startup checks, lifecycle hooks, routes and normal startup recovery in one supervised worker. If the agent restarts, recovery=reconciling with step=reconciling_replacement waits for that original worker. Its verified durable final receipt is delivered without repeating containers or hooks, including a failed deployment whose previous release was restored. Missing or invalid receipts, worker loss or timeout, host reboot before completion, legacy unsupervised operations, data ownership changes and onion provisioning still require support; keep the private journal and do not retry to unblock the queue. This does not add automatic deployment retries, database rollback or zero-downtime traffic switching. Update the agent explicitly before the next deploy.
