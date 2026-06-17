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
curl -fsSL https://impreza.host/agent/install.sh | \
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
  https://impreza.host/agent/releases/latest/impreza-agent-linux-amd64
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
