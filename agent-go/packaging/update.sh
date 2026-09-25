#!/bin/sh
# Update an existing Impreza agent without registering a new identity.
# Default: check only. Use --apply after all deployment operations finish.
# IMPREZA_AGENT_VERSION may pin a numeric release, for example 0.6.0; a
# pinned request always uses the per-release checksum path.
# IMPREZA_AGENT_CHANNEL selects the release channel (stable by default).
# A channel that publishes manifest.signed.json is verified end to end:
# Ed25519 signature, channel, short expiry, strictly increasing sequence
# chained to the last manifest accepted here, and per-artifact digests.
# Once a channel's manifest has been accepted on a server it is mandatory:
# a missing or unverifiable manifest refuses the update instead of
# falling back to same-origin checksums.
set -eu
main() {
    MODE=${1:---check}
    case "$MODE" in --check|--apply) ;; *) echo 'Usage: sh update.sh [--check|--apply]' >&2; exit 2;; esac
    [ "$(id -u)" -eq 0 ] || { echo 'Run as root.' >&2; exit 1; }
    [ "$(uname -s)" = Linux ] || { echo 'Linux is required.' >&2; exit 1; }
    case "$(uname -m)" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) echo 'Unsupported architecture.' >&2; exit 1;; esac
    for tool in curl sha256sum flock systemctl timeout sort awk; do command -v "$tool" >/dev/null || { echo "Missing dependency: $tool" >&2; exit 1; }; done
    BIN=/usr/local/bin/impreza-agent
    [ -f "$BIN" ] && [ ! -L "$BIN" ] && [ -s /etc/impreza-agent/config.toml ] || { echo 'No standard registered agent installation found. Configuration will not be changed.' >&2; exit 1; }
    systemctl cat impreza-agent.service >/dev/null || exit 1
    case "$(systemctl show impreza-agent.service -p ExecStart --value)" in *"/usr/local/bin/impreza-agent run"*) ;; *) echo 'Custom service command: update manually.' >&2; exit 1;; esac
    umask 077
    exec 9>/run/lock/impreza-agent-update.lock
    flock -n 9 || { echo 'Another agent update is in progress.' >&2; exit 1; }
    RELEASE_BASE=${IMPREZA_AGENT_RELEASE_BASE:-https://raw.githubusercontent.com/imprezahost/agent-public/main/releases}
    CHANNEL=${IMPREZA_AGENT_CHANNEL:-stable}
    case "$CHANNEL" in stable|beta) ;; *) echo "Unsupported channel: $CHANNEL" >&2; exit 1;; esac
    ROOT=$RELEASE_BASE/$CHANNEL
    # A SOCKS proxy exists to reach the release mirror over Tor; a v3
    # onion address is self-authenticating, so plain http to an .onion
    # origin is acceptable there and the manifest signature still gates
    # everything installed. Without a proxy, https-only stands.
    CURL_PROXY=
    CURL_PROTO="--proto =https --proto-redir =https"
    if [ -n "${IMPREZA_AGENT_SOCKS_PROXY:-}" ]; then
        CURL_PROXY="--socks5-hostname $IMPREZA_AGENT_SOCKS_PROXY"
        CURL_PROTO=
    fi
    fetch() {
        case "$1" in http://*|https://*)
            # shellcheck disable=SC2086
            curl --fail --silent --show-error --location $CURL_PROTO $CURL_PROXY --connect-timeout 15 --max-time 180 "$1" -o "$2"
        ;; *)
            [ -f "$1" ] || { echo "Local release file missing: $1" >&2; return 1; }
            cp -- "$1" "$2"
        ;; esac
    }
    fetch_status() {
        case "$1" in http://*|https://*)
            # shellcheck disable=SC2086
            curl --silent --show-error --location $CURL_PROTO $CURL_PROXY --connect-timeout 15 --max-time 60 -w '%{http_code}' "$1" -o "$2" 2>/dev/null || printf '000'
        ;; *)
            if [ -f "$1" ]; then cp -- "$1" "$2"; printf '200'; else printf '404'; fi
        ;; esac
    }
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
    STATE_DIR=/var/lib/impreza-agent
    STATE_FILE=$STATE_DIR/update.state.$CHANNEL
    # Accept exactly one ASCII metadata line, with LF, CRLF or no final newline.
    # Never delete embedded control characters or accept one valid line among others.
    read_metadata() {
        LC_ALL=C awk -v kind="$2" '
            NR != 1 { valid = 0; exit }
            {
                sub(/\r$/, "")
                if (kind == "version") valid = ($0 ~ /^[0-9]+\.[0-9]+\.[0-9]+$/)
                else valid = (length($0) == 64 && $0 ~ /^[a-f0-9]+$/)
                value = $0
            }
            END { if (NR != 1 || !valid) exit 1; print value }
        ' "$1"
    }
    # >>> manifest-verify (extracted by tests/test_update_manifest.py)
    # Validate and decode manifest.signed.json, authenticating before
    # interpreting: a first pass only frames the envelope and writes the
    # signed bytes, openssl checks the Ed25519 signature, and only then does
    # a second pass parse the payload, apply the semantic rules and compare
    # the chain with the local state. Both must pass. Every value written to
    # verified.env is format-constrained before it is ever sourced by the shell.
    verify_manifest() {
        # $1 envelope file, $2 state file, $3 work dir
        cat > "$3/verify.py" <<'PYMANIFEST'
import base64, hashlib, json, os, re, sys, time
from datetime import datetime

mode, envelope_path, state_path, out_dir = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
if mode not in ("frame", "validate"):
    fail("verifier mode")
channel = os.environ["IMPREZA_RELEASE_CHANNEL"]
arch = os.environ["IMPREZA_RELEASE_ARCH"]
DOMAIN = b"impreza-agent-release-v1\x00"
MAX_ENVELOPE = 8192
MAX_PAYLOAD = 4096
MAX_VALIDITY = 7 * 24 * 3600
MAX_ARTIFACT = 64 * 1024 * 1024
DEFAULT_KEY = "HismnHv7rcB/AWSrzalz/+c3t921VQ1gmHVJlNx0gfQ="
SEMVER = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")
HEX64 = re.compile(r"^[a-f0-9]{64}$")

def fail(message):
    sys.stderr.write("manifest: %s\n" % message)
    sys.exit(1)

try:
    raw = open(envelope_path, "rb").read(MAX_ENVELOPE + 1)
except OSError:
    fail("envelope unreadable")
if not raw or len(raw) > MAX_ENVELOPE:
    fail("envelope size")
try:
    envelope = json.loads(raw.decode("utf-8"))
except Exception:
    fail("envelope json")
if not isinstance(envelope, dict) or set(envelope) != {"format", "payload", "signature"} or envelope["format"] != 1:
    fail("envelope shape")

def b64field(value, limit, label):
    if not isinstance(value, str):
        fail("envelope %s type" % label)
    try:
        decoded = base64.b64decode(value, validate=True)
    except Exception:
        fail("envelope %s base64" % label)
    if len(decoded) > limit:
        fail("envelope %s size" % label)
    return decoded

payload = b64field(envelope["payload"], MAX_PAYLOAD, "payload")
signature = b64field(envelope["signature"], 64, "signature")
if len(signature) != 64:
    fail("signature size")
envelope_sha = hashlib.sha256(raw).hexdigest()
if mode == "frame":
    # Only the framing above runs before the signature is checked: nothing
    # in the payload is interpreted until openssl has authenticated it.
    key_text = os.environ.get("IMPREZA_RELEASE_PUBLIC_KEY", DEFAULT_KEY)
    try:
        key_bytes = base64.b64decode(key_text, validate=True)
    except Exception:
        fail("trust key base64")
    if len(key_bytes) != 32:
        fail("trust key size")
    with open(os.path.join(out_dir, "msg.bin"), "wb") as handle:
        handle.write(DOMAIN + payload)
    with open(os.path.join(out_dir, "sig.bin"), "wb") as handle:
        handle.write(signature)
    # SubjectPublicKeyInfo DER for a raw Ed25519 key.
    with open(os.path.join(out_dir, "pub.der"), "wb") as handle:
        handle.write(bytes.fromhex("302a300506032b6570032100") + key_bytes)
    with open(os.path.join(out_dir, "envelope.sha256"), "w") as handle:
        handle.write(envelope_sha)
    sys.exit(0)
# validate: the envelope must be the exact bytes the signature covered.
try:
    authenticated = open(os.path.join(out_dir, "envelope.sha256")).read().strip()
except OSError:
    fail("envelope not authenticated")
if authenticated != envelope_sha:
    fail("envelope changed after authentication")
try:
    manifest = json.loads(payload.decode("utf-8"))
except Exception:
    fail("payload json")
required = {"schema", "channel", "version", "seq", "previous", "released_at", "expires_at", "min_agent_version", "artifacts"}
if not isinstance(manifest, dict) or set(manifest) != required or manifest["schema"] != 1:
    fail("payload shape")
if manifest["channel"] != channel:
    fail("channel mismatch")
if not SEMVER.match(manifest["version"]) or not SEMVER.match(manifest["min_agent_version"]):
    fail("version format")
seq = manifest["seq"]
if not isinstance(seq, int) or isinstance(seq, bool) or seq < 1:
    fail("sequence format")
previous = manifest["previous"]
if seq == 1 and previous is not None:
    fail("bootstrap chains a predecessor")
if seq > 1 and (not isinstance(previous, str) or not HEX64.match(previous)):
    fail("predecessor digest format")

def timestamp(value, label):
    if not isinstance(value, str):
        fail("%s type" % label)
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except Exception:
        fail("%s format" % label)
    if parsed.tzinfo is None:
        fail("%s needs a timezone" % label)
    return parsed.timestamp()

now = time.time()
released = timestamp(manifest["released_at"], "released_at")
expires = timestamp(manifest["expires_at"], "expires_at")
if released > now + 300:
    fail("released in the future")
if expires <= now:
    fail("expired")
if expires <= released or expires - released > MAX_VALIDITY:
    fail("validity window")

artifacts = manifest["artifacts"]
if not isinstance(artifacts, dict) or not artifacts:
    fail("artifacts shape")
for name, artifact in artifacts.items():
    if not isinstance(name, str) or not isinstance(artifact, dict) or set(artifact) != {"name", "sha256", "size"}:
        fail("artifact shape")
    if artifact["name"] != "impreza-agent-linux-" + name:
        fail("artifact name")
    if not isinstance(artifact["sha256"], str) or not HEX64.match(artifact["sha256"]):
        fail("artifact digest")
    if not isinstance(artifact["size"], int) or artifact["size"] <= 0 or artifact["size"] > MAX_ARTIFACT:
        fail("artifact size")
if arch not in artifacts:
    fail("no artifact for architecture")

state = None
try:
    with open(state_path, "r") as handle:
        raw_state = handle.read(4097)
        if len(raw_state) > 4096:
            fail("local update state too large")
        state = json.loads(raw_state)
except FileNotFoundError:
    state = None
except Exception:
    fail("local update state unreadable")
if state is not None:
    if not isinstance(state, dict) or set(state) != {"seq", "envelope_sha256", "version", "channel"}:
        fail("local update state invalid")
    if (state["channel"] != channel or not isinstance(state["seq"], int)
            or isinstance(state["seq"], bool) or state["seq"] < 1
            or not isinstance(state["envelope_sha256"], str) or not HEX64.match(state["envelope_sha256"])
            or not isinstance(state["version"], str) or not SEMVER.match(state["version"])):
        fail("local update state channel")
    if seq < state["seq"]:
        fail("rollback: sequence went backwards")
    if seq == state["seq"] and state["envelope_sha256"] != envelope_sha:
        fail("equivocation: sequence reused with a different manifest")
    if seq == state["seq"] + 1 and previous != state["envelope_sha256"]:
        fail("predecessor mismatch")

with open(os.path.join(out_dir, "state.candidate.json"), "w") as handle:
    json.dump({"seq": seq, "envelope_sha256": envelope_sha, "version": manifest["version"], "channel": channel}, handle)
    handle.write("\n")
selected = artifacts[arch]
with open(os.path.join(out_dir, "verified.env"), "w") as handle:
    handle.write("MANIFEST_VERSION=%s\n" % manifest["version"])
    handle.write("MANIFEST_SHA256=%s\n" % selected["sha256"])
    handle.write("MANIFEST_SIZE=%d\n" % selected["size"])
    handle.write("MANIFEST_SEQ=%d\n" % seq)
    handle.write("MANIFEST_MIN_AGENT=%s\n" % manifest["min_agent_version"])
    handle.write("MANIFEST_ENVELOPE_SHA256=%s\n" % envelope_sha)
PYMANIFEST
        IMPREZA_RELEASE_CHANNEL=$CHANNEL IMPREZA_RELEASE_ARCH=$ARCH \
        python3 "$3/verify.py" frame "$1" "$2" "$3" || return 1
        openssl pkey -pubin -inform DER -in "$3/pub.der" -out "$3/pub.pem" >/dev/null 2>&1 || { echo 'manifest: trust key unusable' >&2; return 1; }
        openssl pkeyutl -verify -pubin -inkey "$3/pub.pem" -rawin -in "$3/msg.bin" -sigfile "$3/sig.bin" >/dev/null 2>&1 || { echo 'manifest: signature verification failed' >&2; return 1; }
        IMPREZA_RELEASE_CHANNEL=$CHANNEL IMPREZA_RELEASE_ARCH=$ARCH \
        python3 "$3/verify.py" validate "$1" "$2" "$3" || return 1
        return 0
    }
    # <<< manifest-verify
    record_state() {
        mkdir -p "$STATE_DIR"
        cp -- "$TMP/verify/state.candidate.json" "$STATE_FILE.tmp.$$"
        mv -f "$STATE_FILE.tmp.$$" "$STATE_FILE"
        chmod 0600 "$STATE_FILE"
    }
    MANIFEST_MODE=0
    VERSION=
    if [ -n "${IMPREZA_AGENT_VERSION:-}" ]; then
        printf '%s' "$IMPREZA_AGENT_VERSION" > "$TMP/version"
    else
        mkdir -p "$TMP/verify"
        HTTP=$(fetch_status "$ROOT/manifest.signed.json" "$TMP/manifest.signed.json")
        if [ "$HTTP" = 200 ]; then
            command -v python3 >/dev/null || { echo 'python3 is required to verify the signed release manifest.' >&2; exit 1; }
            command -v openssl >/dev/null || { echo 'openssl is required to verify the signed release manifest.' >&2; exit 1; }
            if ! verify_manifest "$TMP/manifest.signed.json" "$STATE_FILE" "$TMP/verify" 2>>"$TMP/verify.err"; then
                if [ -s "$TMP/verify.err" ]; then sed 's/^/  /' "$TMP/verify.err" >&2; fi
                echo 'The signed release manifest was refused; the installed agent was not changed.' >&2
                exit 1
            fi
            . "$TMP/verify/verified.env"
            MANIFEST_MODE=1
            VERSION=$MANIFEST_VERSION
            echo "Verified manifest: channel $CHANNEL, version $VERSION, sequence $MANIFEST_SEQ (expires with the manifest signing window)."
        elif [ "$HTTP" = 404 ]; then
            if [ -e "$STATE_FILE" ]; then
                echo "Channel $CHANNEL published a signed manifest before (state at $STATE_FILE) but none is available now; refusing unverified fallback." >&2
                exit 1
            fi
            fetch "$ROOT/version.txt" "$TMP/version"
        else
            echo "Unable to check the release manifest (HTTP $HTTP)." >&2
            exit 1
        fi
    fi
    if [ "$MANIFEST_MODE" = 0 ]; then
        VERSION=$(read_metadata "$TMP/version" version) || { echo 'Invalid release version.' >&2; exit 1; }
    fi
    case "${IMPREZA_AGENT_REQUIRE_MANIFEST:-0}" in
        0) ;;
        1)
            [ -z "${IMPREZA_AGENT_VERSION:-}" ] && [ "$MANIFEST_MODE" = 1 ] || {
                echo 'Managed update requires a verified signed manifest; legacy or pinned metadata is refused.' >&2
                exit 1
            }
            ;;
        *) echo 'Invalid manifest requirement.' >&2; exit 1 ;;
    esac
    if [ -n "${IMPREZA_AGENT_EXPECTED_VERSION:-}" ]; then
        printf '%s\n' "$IMPREZA_AGENT_EXPECTED_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || {
            echo 'Invalid expected release version.' >&2; exit 1
        }
        [ "$VERSION" = "$IMPREZA_AGENT_EXPECTED_VERSION" ] || {
            echo 'Channel head changed after the update was requested; retry explicitly.' >&2
            exit 1
        }
    fi
    CURRENT=$(timeout 10 "$BIN" --version | sed -n 's/^impreza-agent version v\{0,1\}\([0-9][0-9.]*\)$/\1/p')
    echo "Installed: ${CURRENT:-unknown}; available: $VERSION (channel $CHANNEL)"
    [ "$MODE" = --apply ] || exit 0
    if [ -n "$CURRENT" ] && [ "$CURRENT" != "$VERSION" ] && [ "$(printf '%s\n%s\n' "$CURRENT" "$VERSION" | sort -V | head -1)" = "$VERSION" ]; then echo 'Downgrade refused.' >&2; exit 1; fi
    NAME=impreza-agent-linux-$ARCH
    if [ "$MANIFEST_MODE" = 1 ]; then
        fetch "$ROOT/$VERSION/$NAME" "$TMP/candidate"
        SIZE=$(wc -c < "$TMP/candidate" | tr -d ' ')
        [ "$SIZE" = "$MANIFEST_SIZE" ] || { echo 'Release size differs from the signed manifest; installed agent was not changed.' >&2; exit 1; }
        printf '%s  %s\n' "$MANIFEST_SHA256" "$TMP/candidate" | sha256sum -c - >/dev/null || { echo 'Checksum differs from the signed manifest; installed agent was not changed.' >&2; exit 1; }
    else
        fetch "$ROOT/$VERSION/$NAME.sha256" "$TMP/checksum"
        HASH=$(read_metadata "$TMP/checksum" checksum) || { echo 'Invalid release checksum.' >&2; exit 1; }
        fetch "$ROOT/$VERSION/$NAME" "$TMP/candidate"
        printf '%s  %s\n' "$HASH" "$TMP/candidate" | sha256sum -c - >/dev/null || { echo 'Checksum mismatch; installed agent was not changed.' >&2; exit 1; }
    fi
    chmod 0755 "$TMP/candidate"
    [ "$(timeout 10 "$TMP/candidate" --version)" = "impreza-agent version $VERSION" ] || { echo 'Release version mismatch.' >&2; exit 1; }
    if [ "$(sha256sum "$TMP/candidate" | cut -d ' ' -f 1)" = "$(sha256sum "$BIN" | cut -d ' ' -f 1)" ]; then
        if [ "$MANIFEST_MODE" = 1 ]; then record_state; fi
        echo 'Agent is already current.'
        exit 0
    fi
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
    if [ "$MANIFEST_MODE" = 1 ]; then record_state; fi
    CHANGED=0
    echo "Agent $VERSION is running. Confirm its new heartbeat in the portal. Credentials, configuration and app containers were preserved."
}
main "$@"
