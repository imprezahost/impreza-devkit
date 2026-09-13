#!/bin/sh
# impreza-agent installer — curl|sh entry point.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/imprezahost/agent-public/main/install.sh | \
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

if [ -s /etc/impreza-agent/config.toml ]; then
    die "agent already registered; use https://raw.githubusercontent.com/imprezahost/agent-public/main/update.sh with --apply"
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

# ─── curl (distro uniformity) ───────────────────────────────────────────
# Everything below (Docker bootstrap, binary download) needs curl, but
# minimal Debian images ship without it — Ubuntu cloud images and the
# RHEL family (Alma/Rocky, via curl-minimal) include it. Someone running
# this script via `wget -qO- | sh` would otherwise die mid-install.
if ! command -v curl >/dev/null 2>&1; then
    say "curl not found — installing via package manager"
    if command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get -o DPkg::Lock::Timeout=300 update -qq || true
        apt-get -o DPkg::Lock::Timeout=300 install -y -qq curl ca-certificates || true
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y -q --allowerasing curl ca-certificates || true
    elif command -v yum >/dev/null 2>&1; then
        yum install -y -q curl ca-certificates || true
    fi
    command -v curl >/dev/null 2>&1 || die "curl unavailable and could not be installed"
fi

# ─── Docker ─────────────────────────────────────────────────────────────
# The Docker executor needs `docker` + `docker compose` (v2 plugin).
# Skip with IMPREZA_AGENT_SKIP_DOCKER=1 if Docker is provisioned by an
# external mechanism (e.g. cloud-init), or if testing without it.

# install_docker — install Docker CE across distros. The official
# get.docker.com convenience script REFUSES RHEL rebuilds (it exits 1 with
# "ERROR: Unsupported distribution 'almalinux'" / 'rocky'), which silently
# broke every Alma/Rocky provision. So on the RHEL family we add Docker's
# CE repo directly: the CentOS repo resolves $releasever to el9 and serves
# Alma/Rocky/RHEL/Oracle identically (validated on AlmaLinux 9.7 →
# docker-ce 29.x + compose v2 plugin). Debian/Ubuntu and anything else
# get.docker.com supports keep the convenience script.
install_docker() {
    _id=""; _like=""
    if [ -r /etc/os-release ]; then
        _id=$(. /etc/os-release 2>/dev/null; printf '%s' "${ID:-}")
        _like=$(. /etc/os-release 2>/dev/null; printf '%s' "${ID_LIKE:-}")
    fi
    case " ${_id} ${_like} " in
        *" rhel "*|*" centos "*|*" almalinux "*|*" rocky "*|*" fedora "*|*" ol "*|*" oracle "*)
            _repo=centos
            [ "${_id}" = "fedora" ] && _repo=fedora
            if command -v dnf >/dev/null 2>&1; then
                dnf -y install dnf-plugins-core || return 1
                dnf config-manager --add-repo "https://download.docker.com/linux/${_repo}/docker-ce.repo" || return 1
                dnf -y install docker-ce docker-ce-cli containerd.io docker-compose-plugin || return 1
            elif command -v yum >/dev/null 2>&1; then
                yum -y install yum-utils || return 1
                yum-config-manager --add-repo "https://download.docker.com/linux/${_repo}/docker-ce.repo" || return 1
                yum -y install docker-ce docker-ce-cli containerd.io docker-compose-plugin || return 1
            else
                return 1
            fi
            ;;
        *)
            # Debian/Ubuntu and others supported by the convenience script.
            curl -fsSL https://get.docker.com | sh || return 1
            ;;
    esac
}

if [ "${IMPREZA_AGENT_SKIP_DOCKER:-0}" != "1" ]; then
    if ! command -v docker >/dev/null 2>&1; then
        say "Docker not found — installing Docker CE"
        if ! install_docker; then
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
# Download public release binaries; distributors may override the release base.
RELEASE_BASE="${IMPREZA_AGENT_RELEASE_BASE:-https://raw.githubusercontent.com/imprezahost/agent-public/main/releases}"
BINARY_URL="$RELEASE_BASE/$CHANNEL/$VERSION/impreza-agent-linux-$ARCH"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

say "downloading impreza-agent ($CHANNEL/$VERSION, linux-$ARCH)"
# Custom UA so Cloudflare Bot Fight Mode doesn't 403 us. The post-prov
# prelude exports IMPREZA_AGENT_USER_AGENT; if absent (manual curl|sh
# from a customer SSH session) we fall back to a sensible default.
IMPREZA_UA="${IMPREZA_AGENT_USER_AGENT:-impreza-agent-installer/1.0 (https://impreza.host)}"
if ! curl -fsSL -A "$IMPREZA_UA" -o "$TMP/impreza-agent" "$BINARY_URL"; then
    die "download failed: $BINARY_URL"
fi
if ! curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' -o "$TMP/checksum" "$BINARY_URL.sha256"; then
    die "release checksum download failed"
fi
HASH=$(cat "$TMP/checksum")
printf '%s\n' "$HASH" | grep -Eq '^[a-f0-9]{64}$' || die "invalid release checksum"
printf '%s  %s\n' "$HASH" "$TMP/impreza-agent" | sha256sum -c - >/dev/null || die "release checksum mismatch"
chmod +x "$TMP/impreza-agent"

# The agent pulls the public Caddy image when deploying applications.

# ─── Install binary ─────────────────────────────────────────────────────
say "installing binary to /usr/local/bin/impreza-agent"
install -m 0755 "$TMP/impreza-agent" /usr/local/bin/impreza-agent

# ─── Create required directories ────────────────────────────────────────
# The systemd unit installed below uses ReadWritePaths= which requires
# every listed directory to exist BEFORE the service starts — otherwise
# systemd fails with status=226/NAMESPACE ("Failed to set up mount
# namespacing: /var/log/impreza-agent: No such file or directory") and
# the service enters a restart-loop. install.sh never created these on
# fresh hosts, so the first start always failed.
mkdir -p /etc/impreza-agent /var/lib/impreza-agent /var/log/impreza-agent
chmod 0750 /var/lib/impreza-agent /var/log/impreza-agent

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
# Idempotent: when /etc/impreza-agent/config.toml already exists, we're
# resuming a previous install that succeeded at the bootstrap step but
# failed somewhere after (e.g. systemd-unit dir missing). Calling
# bootstrap again would fail with "config already exists" and the
# whole install.sh would die. Skip bootstrap in that case and just
# enable+start below — the existing credentials remain valid.
if [ -f /etc/impreza-agent/config.toml ]; then
    say "config already exists at /etc/impreza-agent/config.toml — skipping bootstrap"
else
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
