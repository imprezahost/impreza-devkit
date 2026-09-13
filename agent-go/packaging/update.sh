#!/bin/sh
# Update an existing Impreza agent without registering a new identity.
# Default: check only. Use --apply after all deployment operations finish.
# IMPREZA_AGENT_VERSION may pin a numeric release, for example 0.6.0.
set -eu
main() {
    MODE=${1:---check}
    case "$MODE" in --check|--apply) ;; *) echo 'Usage: sh update.sh [--check|--apply]' >&2; exit 2;; esac
    [ "$(id -u)" -eq 0 ] || { echo 'Run as root.' >&2; exit 1; }
    [ "$(uname -s)" = Linux ] || { echo 'Linux is required.' >&2; exit 1; }
    case "$(uname -m)" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) echo 'Unsupported architecture.' >&2; exit 1;; esac
    for tool in curl sha256sum flock systemctl timeout sort; do command -v "$tool" >/dev/null || { echo "Missing dependency: $tool" >&2; exit 1; }; done
    BIN=/usr/local/bin/impreza-agent
    [ -f "$BIN" ] && [ ! -L "$BIN" ] && [ -s /etc/impreza-agent/config.toml ] || { echo 'No standard registered agent installation found. Configuration will not be changed.' >&2; exit 1; }
    systemctl cat impreza-agent.service >/dev/null || exit 1
    case "$(systemctl show impreza-agent.service -p ExecStart --value)" in *"/usr/local/bin/impreza-agent run"*) ;; *) echo 'Custom service command: update manually.' >&2; exit 1;; esac
    umask 077
    exec 9>/run/lock/impreza-agent-update.lock
    flock -n 9 || { echo 'Another agent update is in progress.' >&2; exit 1; }
    ROOT=https://raw.githubusercontent.com/imprezahost/agent-public/main/releases/stable
    fetch() { curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 180 "$1" -o "$2"; }
    TMP=$(mktemp -d /usr/local/bin/.impreza-agent-update.XXXXXX)
    CHANGED=0
    finish() {
        code=$?
        trap - EXIT HUP INT TERM
        if [ "$CHANGED" = 1 ]; then
            echo 'Update failed; restoring the previous agent.' >&2
            if mv -f "$TMP/previous" "$BIN" && systemctl restart impreza-agent.service; then
                sleep 3
                systemctl is-active --quiet impreza-agent.service || echo 'Previous binary restored but service needs attention.' >&2
            else
                echo "Recovery failed. Preserve $TMP/previous for manual recovery." >&2
                exit 1
            fi
        fi
        rm -rf -- "$TMP"
        exit "$code"
    }
    trap finish EXIT
    trap 'exit 130' HUP INT TERM
    if [ -n "${IMPREZA_AGENT_VERSION:-}" ]; then VERSION=$IMPREZA_AGENT_VERSION; else fetch "$ROOT/version.txt" "$TMP/version"; VERSION=$(cat "$TMP/version"); fi
    printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo 'Invalid release version.' >&2; exit 1; }
    CURRENT=$(timeout 10 "$BIN" --version | sed -n 's/^impreza-agent version v\{0,1\}\([0-9][0-9.]*\)$/\1/p')
    echo "Installed: ${CURRENT:-unknown}; available: $VERSION"
    [ "$MODE" = --apply ] || exit 0
    if [ -n "$CURRENT" ] && [ "$CURRENT" != "$VERSION" ] && [ "$(printf '%s\n%s\n' "$CURRENT" "$VERSION" | sort -V | head -1)" = "$VERSION" ]; then echo 'Downgrade refused.' >&2; exit 1; fi
    NAME=impreza-agent-linux-$ARCH
    fetch "$ROOT/$VERSION/$NAME.sha256" "$TMP/checksum"
    HASH=$(cat "$TMP/checksum")
    printf '%s\n' "$HASH" | grep -Eq '^[a-f0-9]{64}$' || { echo 'Invalid release checksum.' >&2; exit 1; }
    fetch "$ROOT/$VERSION/$NAME" "$TMP/candidate"
    printf '%s  %s\n' "$HASH" "$TMP/candidate" | sha256sum -c - >/dev/null || { echo 'Checksum mismatch; installed agent was not changed.' >&2; exit 1; }
    chmod 0755 "$TMP/candidate"
    [ "$(timeout 10 "$TMP/candidate" --version)" = "impreza-agent version $VERSION" ] || { echo 'Release version mismatch.' >&2; exit 1; }
    if [ "$(sha256sum "$BIN" | cut -d ' ' -f 1)" = "$HASH" ]; then echo 'Agent is already current.'; exit 0; fi
    echo 'Restarting only the agent. Run this update only when deployment operations are idle.'
    cp -p "$BIN" "$TMP/previous"
    # Same filesystem rename keeps the executable replacement atomic.
    CHANGED=1
    systemctl stop impreza-agent.service
    mv -f "$TMP/candidate" "$BIN"
    systemctl reset-failed impreza-agent.service || true
    systemctl start impreza-agent.service
    PID=$(systemctl show impreza-agent.service -p MainPID --value)
    [ "$PID" -gt 0 ] || exit 1
    count=0
    while [ "$count" -lt 10 ]; do
        sleep 1
        systemctl is-active --quiet impreza-agent.service || exit 1
        [ "$(systemctl show impreza-agent.service -p MainPID --value)" = "$PID" ] || exit 1
        count=$((count + 1))
    done
    CHANGED=0
    echo "Agent $VERSION is running. Confirm its new heartbeat in the portal. Credentials, configuration and app containers were preserved."
}
main "$@"
