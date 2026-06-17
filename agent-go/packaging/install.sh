#!/bin/sh
# impreza-agent installer — curl|sh entry point.
#
# Usage:
#   curl -fsSL https://impreza.host/agent/install.sh | \
#     IMPREZA_BOOTSTRAP=bst_xxxxxxxxxxxxxxxx sh
#
# What this does:
#   1. Detects OS + arch (Linux only; refuses on anything else).
#   2. Downloads the matching impreza-agent binary into /usr/local/bin.
#   3. Installs the systemd unit at /etc/systemd/system.
#   4. Runs `impreza-agent bootstrap` with the supplied IMPREZA_BOOTSTRAP token.
#   5. Enables + starts the service.
#
# Environment variables:
#   IMPREZA_BOOTSTRAP            — one-time bootstrap token (required)
#   IMPREZA_AGENT_VERSION        — pin a specific version (default: "latest")
#   IMPREZA_AGENT_CHANNEL        — release channel: "stable" | "beta" (default: stable)
#   IMPREZA_AGENT_CONTROL_PLANE  — override the control-plane URL (default: prod)
#   IMPREZA_AGENT_USE_TOR        — set to "1" to route everything via Tor

set -eu

# ─── Logging helpers ────────────────────────────────────────────────────
say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ─── Preflight ──────────────────────────────────────────────────────────
[ "$(id -u)" -eq 0 ] || die "must run as root (try: sudo IMPREZA_BOOTSTRAP=… sh -)"

if [ -z "${IMPREZA_BOOTSTRAP:-}" ]; then
    die "IMPREZA_BOOTSTRAP is required — issue a token from the panel and re-run."
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
[ "$OS" = "linux" ] || die "only Linux is supported (got $OS)"

case "$(uname -m)" in
    x86_64|amd64)   ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
esac

VERSION="${IMPREZA_AGENT_VERSION:-latest}"
CHANNEL="${IMPREZA_AGENT_CHANNEL:-stable}"
case "$CHANNEL" in stable|beta) ;; *) die "invalid channel: $CHANNEL" ;; esac

# ─── Docker ─────────────────────────────────────────────────────────────
# The Docker executor needs `docker` + `docker compose` (v2 plugin).
# Skip with IMPREZA_AGENT_SKIP_DOCKER=1 if Docker is provisioned by an
# external mechanism (e.g. cloud-init), or if testing without it.
if [ "${IMPREZA_AGENT_SKIP_DOCKER:-0}" != "1" ]; then
    if ! command -v docker >/dev/null 2>&1; then
        say "Docker not found — installing via the official get.docker.com script"
        if ! curl -fsSL https://get.docker.com | sh; then
            die "Docker install failed. Set IMPREZA_AGENT_SKIP_DOCKER=1 if Docker is provisioned elsewhere."
        fi
        systemctl enable --now docker || true
    fi
    # Verify `docker compose` (v2 plugin) is available — old docker-compose
    # standalone won't work with the agent's executor.
    if ! docker compose version >/dev/null 2>&1; then
        warn "docker is installed but 'docker compose' (v2 plugin) is missing."
        warn "Most modern distros bundle it; install docker-compose-plugin manually if needed."
    fi
fi

# ─── Download ───────────────────────────────────────────────────────────
RELEASE_BASE="https://impreza.host/agent/releases"
BINARY_URL="$RELEASE_BASE/$CHANNEL/$VERSION/impreza-agent-linux-$ARCH"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

say "downloading impreza-agent ($CHANNEL/$VERSION, linux-$ARCH)"
if ! curl -fsSL -o "$TMP/impreza-agent" "$BINARY_URL"; then
    die "download failed: $BINARY_URL"
fi
chmod +x "$TMP/impreza-agent"

# ─── Install binary ─────────────────────────────────────────────────────
say "installing binary to /usr/local/bin/impreza-agent"
install -m 0755 "$TMP/impreza-agent" /usr/local/bin/impreza-agent

# ─── Install systemd unit ───────────────────────────────────────────────
say "installing systemd unit"
cat >/etc/systemd/system/impreza-agent.service <<'UNIT'
[Unit]
Description=Impreza Platform Agent
Documentation=https://docs.imprezahost.com/agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/impreza-agent run
Restart=on-failure
RestartSec=5s

User=root
Group=root

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=/etc/impreza-agent /var/lib/impreza-agent /var/log/impreza-agent

LimitNOFILE=65536

StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload

# ─── Bootstrap ──────────────────────────────────────────────────────────
say "registering agent with control plane"
BOOTSTRAP_ARGS="--token $IMPREZA_BOOTSTRAP"
[ -n "${IMPREZA_AGENT_CONTROL_PLANE:-}" ] && \
    BOOTSTRAP_ARGS="$BOOTSTRAP_ARGS --control-plane $IMPREZA_AGENT_CONTROL_PLANE"
[ "${IMPREZA_AGENT_USE_TOR:-0}" = "1" ] && \
    BOOTSTRAP_ARGS="$BOOTSTRAP_ARGS --tor"

# shellcheck disable=SC2086
if ! /usr/local/bin/impreza-agent bootstrap $BOOTSTRAP_ARGS; then
    die "bootstrap failed — token may be expired or already consumed"
fi

# ─── Enable + start ─────────────────────────────────────────────────────
say "enabling and starting impreza-agent.service"
systemctl enable --now impreza-agent

# Give the daemon a moment to make its first heartbeat so `status`
# below is meaningful.
sleep 2
systemctl --no-pager status impreza-agent | head -n 5 || true

cat <<DONE

Installation complete.

  Logs:        journalctl -u impreza-agent -f
  Diagnostics: impreza-agent doctor
  Config:      /etc/impreza-agent/config.toml

DONE
