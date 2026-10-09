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

# install_docker — install Docker CE from Docker's own distribution
# repositories, never by piping a remote script into a shell (the
# old get.docker.com pipe executed unverified bytes from the network, and
# it refused RHEL rebuilds anyway, which silently broke Alma/Rocky
# provisions). The RHEL family uses the CentOS repo ($releasever → el9,
# serving Alma/Rocky/RHEL/Oracle identically; validated on AlmaLinux 9.7 →
# docker-ce 29.x + compose v2 plugin); Debian/Ubuntu use Docker's apt
# repository. Anything else stops with a clear message.
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
            # Never pipe a remote script into a shell. Debian/Ubuntu
            # get Docker's own apt repository (the same setup the RHEL
            # branch does with dnf); anything else stops here instead of
            # executing unverified bytes from the network.
            # Read the fields in subshells: sourcing os-release here would
            # overwrite VERSION (the agent version to install) with the
            # distribution's version string.
            _cn=$(. /etc/os-release 2>/dev/null; printf '%s' "${VERSION_CODENAME:-}")
            case "$_id" in
                debian|ubuntu)
                    export DEBIAN_FRONTEND=noninteractive
                    apt-get -o DPkg::Lock::Timeout=300 update -qq || true
                    apt-get -o DPkg::Lock::Timeout=300 install -y -qq ca-certificates curl gnupg || return 1
                    install -m 0755 -d /etc/apt/keyrings
                    curl -fsSL "https://download.docker.com/linux/$_id/gpg" -o /etc/apt/keyrings/docker.asc || return 1
                    chmod a+r /etc/apt/keyrings/docker.asc
                    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$_id $_cn stable" > /etc/apt/sources.list.d/docker.list
                    apt-get -o DPkg::Lock::Timeout=300 update -qq || return 1
                    apt-get -o DPkg::Lock::Timeout=300 install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || return 1
                    ;;
                *)
                    echo "No Docker packages for this distribution: ID=$_id (install Docker yourself and re-run with IMPREZA_AGENT_SKIP_DOCKER=1)" >&2
                    return 1
                    ;;
            esac
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
# An unpinned install resolves the channel's SIGNED manifest —
# Ed25519 signature, channel, short expiry, sequence chain and per-artifact
# digests, the exact verification update.sh applies (the verifier below is
# the same block, byte for byte) — and downloads the exact version the
# manifest pins, checked against the manifest's digest and size. A missing
# or refused manifest REFUSES the install: a same-origin checksum fallback
# would re-open the hole the manifest closes. An explicit
# IMPREZA_AGENT_VERSION pin keeps update.sh's documented per-release
# checksum path (the manifest only carries the current release).
# Distributors may still override the release base.
RELEASE_BASE="${IMPREZA_AGENT_RELEASE_BASE:-https://raw.githubusercontent.com/imprezahost/agent-public/main/releases}"
ROOT="$RELEASE_BASE/$CHANNEL"
STATE_DIR=/var/lib/impreza-agent
STATE_FILE=$STATE_DIR/update.state.$CHANNEL

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# fetch_status mirrors update.sh: https-only for network origins, plain
# file copy for distributor-local trees (used by the release tests).
fetch_status() {
    case "$1" in
        http://*|https://*)
            curl --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 60 -w '%{http_code}' -A "${IMPREZA_UA:-impreza-agent-installer/1.0}" "$1" -o "$2" 2>/dev/null || printf '000'
        ;;
        *)
            if [ -f "$1" ]; then cp -- "$1" "$2"; printf '200'; else printf '404'; fi
        ;;
    esac
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

mode = sys.argv[1]

def fail(message):
    sys.stderr.write("manifest: %s\n" % message)
    sys.exit(1)

# RFC 8032 Ed25519 verification, used only when the installed openssl has
# no -rawin (1.1.1: Ubuntu 20.04, Debian 11 — there is no raw-EdDSA CLI
# path there at all). The bytes checked are exactly the ones the openssl
# path checks. Public data only: no secret ever reaches this code, so the
# reference implementation's non-constant-time arithmetic leaks nothing.
DOMAIN = b"impreza-agent-release-v1\x00"
p = 2**255 - 19
q = 2**252 + 27742317777372353535851937790883648493
SPKI_ED25519 = bytes.fromhex("302a300506032b6570032100")

def _sha512_modq(data):
    return int.from_bytes(hashlib.sha512(data).digest(), "little") % q

def _modp_inv(x):
    return pow(x, p - 2, p)

_d = -121665 * _modp_inv(121666) % p

def _point_add(P, Q):
    A = (P[1] - P[0]) * (Q[1] - Q[0]) % p
    B = (P[1] + P[0]) * (Q[1] + Q[0]) % p
    C = 2 * P[3] * Q[3] * _d % p
    D = 2 * P[2] * Q[2] % p
    E, F, G, H = B - A, D - C, D + C, B + A
    return (E * F % p, G * H % p, F * G % p, E * H % p)

def _point_mul(s, P):
    Q = (0, 1, 1, 0)
    while s > 0:
        if s & 1:
            Q = _point_add(Q, P)
        P = _point_add(P, P)
        s >>= 1
    return Q

def _point_equal(P, Q):
    if (P[0] * Q[2] - Q[0] * P[2]) % p != 0:
        return False
    if (P[1] * Q[2] - Q[1] * P[2]) % p != 0:
        return False
    return True

def _point_decompress(s):
    if len(s) != 32:
        return None
    y = int.from_bytes(s, "little")
    sign = y >> 255
    y &= (1 << 255) - 1
    if y >= p:
        return None
    u = (y * y - 1) % p
    v = (_d * y * y + 1) % p
    x = pow(u * _modp_inv(v), (p + 3) // 8, p)
    if (x * x - u * _modp_inv(v)) % p != 0:
        x = x * pow(2, (p - 1) // 4, p) % p
    if (x * x - u * _modp_inv(v)) % p != 0:
        return None
    if x == 0 and sign:
        return None
    if x & 1 != sign:
        x = p - x
    return (x, y, 1, x * y % p)

_BASE = _point_decompress(bytes.fromhex("5866666666666666666666666666666666666666666666666666666666666666"))

def _read_file(path, limit, label):
    try:
        data = open(path, "rb").read(limit + 1)
    except OSError:
        fail("%s unreadable" % label)
    if len(data) > limit:
        fail("%s size" % label)
    return data

def ed25519_verify_files(pub_der, msg_bin, sig_bin):
    spki = _read_file(pub_der, 44, "trust key")
    if len(spki) != 44 or not spki.startswith(SPKI_ED25519):
        fail("trust key unusable")
    public = spki[-32:]
    message = _read_file(msg_bin, 4096 + len(DOMAIN), "signed payload")
    signature = _read_file(sig_bin, 64, "signature")
    if len(signature) != 64:
        fail("signature size")
    r_bytes, s_bytes = signature[:32], signature[32:]
    R = _point_decompress(r_bytes)
    A = _point_decompress(public)
    if R is None or A is None:
        fail("signature verification failed")
    S = int.from_bytes(s_bytes, "little")
    if S >= q:
        fail("signature verification failed")
    k = _sha512_modq(r_bytes + public + message)
    if not _point_equal(_point_mul(S, _BASE), _point_add(R, _point_mul(k, A))):
        fail("signature verification failed")

if mode == "ed25519":
    if len(sys.argv) != 5:
        fail("verifier arguments")
    ed25519_verify_files(sys.argv[2], sys.argv[3], sys.argv[4])
    sys.exit(0)

envelope_path, state_path, out_dir = sys.argv[2], sys.argv[3], sys.argv[4]
if mode not in ("frame", "validate"):
    fail("verifier mode")
channel = os.environ["IMPREZA_RELEASE_CHANNEL"]
arch = os.environ["IMPREZA_RELEASE_ARCH"]
MAX_ENVELOPE = 8192
MAX_PAYLOAD = 4096
MAX_VALIDITY = 7 * 24 * 3600
MAX_ARTIFACT = 64 * 1024 * 1024
DEFAULT_KEY = "HismnHv7rcB/AWSrzalz/+c3t921VQ1gmHVJlNx0gfQ="
SEMVER = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")
HEX64 = re.compile(r"^[a-f0-9]{64}$")

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
        # The Ed25519 check runs in openssl when it knows -rawin (OpenSSL 3)
        # and in verify.py's RFC 8032 verifier when it does not (1.1.1:
        # Ubuntu 20.04, Debian 11 have no raw-EdDSA CLI path). Either way
        # a refusal names what failed instead of hiding the cause.
        if openssl pkeyutl -help 2>&1 | grep -q -e '-rawin'; then
            openssl pkey -pubin -inform DER -in "$3/pub.der" -out "$3/pub.pem" 2>"$3/key.err" || {
                echo 'manifest: trust key unusable' >&2
                sed 's/^/  /' "$3/key.err" >&2
                return 1
            }
            if ! openssl pkeyutl -verify -pubin -inkey "$3/pub.pem" -rawin -in "$3/msg.bin" -sigfile "$3/sig.bin" >"$3/sig.out" 2>"$3/sig.err"; then
                echo 'manifest: signature verification failed' >&2
                sed 's/^/  openssl: /' "$3/sig.err" >&2
                return 1
            fi
        else
            # Said out loud, not hidden: on these systems the signature is
            # checked by the built-in RFC 8032 verifier because the openssl
            # binary cannot. Stdout on purpose — verify_manifest's stderr is
            # captured and only shown on refusal, and the customer must see
            # why the built-in checker is in play when the update succeeds.
            echo 'manifest: this OpenSSL has no raw Ed25519 support; verifying with the built-in RFC 8032 checker'
            IMPREZA_RELEASE_CHANNEL=$CHANNEL IMPREZA_RELEASE_ARCH=$ARCH \
            python3 "$3/verify.py" ed25519 "$3/pub.der" "$3/msg.bin" "$3/sig.bin" || return 1
        fi
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
if [ "$VERSION" = "latest" ]; then
    command -v python3 >/dev/null || die "python3 is required to verify the signed release manifest"
    command -v openssl >/dev/null || die "openssl is required to verify the signed release manifest"
    HTTP=$(fetch_status "$ROOT/manifest.signed.json" "$TMP/manifest.signed.json")
    if [ "$HTTP" != "200" ]; then
        die "the $CHANNEL channel must publish its signed release manifest (HTTP $HTTP); an unpinned install refuses unverified downloads"
    fi
    mkdir -p "$TMP/verify"
    if ! verify_manifest "$TMP/manifest.signed.json" "$STATE_FILE" "$TMP/verify" 2>>"$TMP/verify.err"; then
        if [ -s "$TMP/verify.err" ]; then sed 's/^/  /' "$TMP/verify.err" >&2; fi
        die "the signed release manifest was refused; nothing was installed"
    fi
    . "$TMP/verify/verified.env"
    MANIFEST_MODE=1
    VERSION=$MANIFEST_VERSION
else
    printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || die "invalid pinned version (expected semantic version like 0.6.25)"
fi
BINARY_URL="$ROOT/$VERSION/impreza-agent-linux-$ARCH"

say "downloading impreza-agent ($CHANNEL/$VERSION, linux-$ARCH)"
# Custom UA so Cloudflare Bot Fight Mode doesn't 403 us. The post-prov
# prelude exports IMPREZA_AGENT_USER_AGENT; if absent (manual curl|sh
# from a customer SSH session) we fall back to a sensible default.
IMPREZA_UA="${IMPREZA_AGENT_USER_AGENT:-impreza-agent-installer/1.0 (https://impreza.host)}"
if ! curl -fsSL -A "$IMPREZA_UA" -o "$TMP/impreza-agent" "$BINARY_URL"; then
    die "download failed: $BINARY_URL"
fi
if [ "$MANIFEST_MODE" = 1 ]; then
    [ "$(wc -c <"$TMP/impreza-agent")" -eq "$MANIFEST_SIZE" ] || die "release size differs from the signed manifest; nothing was installed"
    printf '%s  %s\n' "$MANIFEST_SHA256" "$TMP/impreza-agent" | sha256sum -c - >/dev/null || die "checksum differs from the signed manifest; nothing was installed"
    # Anchor the update chain from birth: the first update.sh run compares
    # against the manifest this install actually verified.
    record_state
    say "verified signed manifest: version $MANIFEST_VERSION, sequence $MANIFEST_SEQ"
else
    if ! curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' -o "$TMP/checksum" "$BINARY_URL.sha256"; then
        die "release checksum download failed"
    fi
    HASH=$(cat "$TMP/checksum")
    printf '%s\n' "$HASH" | grep -Eq '^[a-f0-9]{64}$' || die "invalid release checksum"
    printf '%s  %s\n' "$HASH" "$TMP/impreza-agent" | sha256sum -c - >/dev/null || die "release checksum mismatch"
fi
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

# The boot restore of the ingress allowlists (see the unit's comments);
# enabled now, inert until the platform stores a restricted allowlist.
cat >/etc/systemd/system/impreza-agent-ingress.service <<'UNIT'
[Unit]
Description=Impreza ingress allowlists (restored before Docker publishes ports)
Documentation=https://docs.imprezahost.com/agent
# Render the stored per-deployment allowlists before Docker starts the
# containers, so no restricted port is reachable in the boot window, and
# again before every Docker (re)start. After the host firewall managers so
# their startup does not reorder or flush the rules afterwards. Ordering
# only: a failure here never blocks Docker; the agent retries and reports.
DefaultDependencies=no
After=local-fs.target firewalld.service ufw.service
Before=docker.service
ConditionPathExists=/var/lib/impreza-agent/ingress.json

[Service]
Type=oneshot
ExecStart=/usr/local/bin/impreza-agent ingress restore
User=root
Group=root
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=/var/lib/impreza-agent

[Install]
WantedBy=multi-user.target docker.service
UNIT

systemctl daemon-reload
systemctl enable impreza-agent-ingress.service >/dev/null 2>&1 || true

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
