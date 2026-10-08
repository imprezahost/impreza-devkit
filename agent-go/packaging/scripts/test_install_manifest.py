"""Behavioral tests for the installer's manifest verifier.

Run: python agent-go/packaging/scripts/test_install_manifest.py

Extracts the verifier exactly as install.sh ships it (the same heredoc
update.sh carries — pinned byte-for-byte by TestInstallVerifierMatchesUpdater)
and drives the manifest cases an unpinned install must answer: a valid
envelope passes; tampered payloads, foreign keys, wrong channels, expired
windows and sequence rollbacks are refused. Signing is the public RFC 8032
algorithm with an ephemeral key — no release secret is involved.
"""
import base64, hashlib, json, os, pathlib, subprocess, sys, tempfile, unittest
from datetime import datetime, timedelta, timezone

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / 'install.sh').read_text()
DOMAIN = b"impreza-agent-release-v1\x00"
P = 2**255 - 19
Q = 2**252 + 27742317777372353535851937790883648493
_D = -121665 * pow(121666, P - 2, P) % P


def _inv(x):
    return pow(x, P - 2, P)


def _padd(Ph, Qh):
    A = (Ph[1] - Ph[0]) * (Qh[1] - Qh[0]) % P
    B = (Ph[1] + Ph[0]) * (Qh[1] + Qh[0]) % P
    C = 2 * Ph[3] * Qh[3] * _D % P
    D = 2 * Ph[2] * Qh[2] % P
    E, F, G, H = B - A, D - C, D + C, B + A
    return (E * F % P, G * H % P, F * G % P, E * H % P)


def _pmul(s, Ph):
    Qh = (0, 1, 1, 0)
    while s > 0:
        if s & 1:
            Qh = _padd(Qh, Ph)
        Ph = _padd(Ph, Ph)
        s >>= 1
    return Qh


def _pcompress(Ph):
    zinv = _inv(Ph[2])
    x = Ph[0] * zinv % P
    y = Ph[1] * zinv % P
    return int.to_bytes(y | ((x & 1) << 255), 32, "little")


def _pdecompress(s):
    y = int.from_bytes(s, "little")
    sign = y >> 255
    y &= (1 << 255) - 1
    if y >= P:
        return None
    u = (y * y - 1) % P
    v = (_D * y * y + 1) % P
    x = pow(u * _inv(v), (P + 3) // 8, P)
    if (x * x - u * _inv(v)) % P != 0:
        x = x * pow(2, (P - 1) // 4, P) % P
    if (x * x - u * _inv(v)) % P != 0:
        return None
    if x == 0 and sign:
        return None
    if x & 1 != sign:
        x = P - x
    return (x, y, 1, x * y % P)


_BASE = _pdecompress(bytes.fromhex("5866666666666666666666666666666666666666666666666666666666666666"))


def sign(seed, message):
    h = hashlib.sha512(seed).digest()
    a = int.from_bytes(h[:32], "little")
    a &= (1 << 254) - 8
    a |= (1 << 254)
    prefix = h[32:]
    big_a = _pcompress(_pmul(a, _BASE))
    r = int.from_bytes(hashlib.sha512(prefix + message).digest(), "little") % Q
    r_point = _pcompress(_pmul(r, _BASE))
    k = int.from_bytes(hashlib.sha512(r_point + big_a + message).digest(), "little") % Q
    s = (r + k * a) % Q
    return r_point + int.to_bytes(s, 32, "little")


class Key:
    def __init__(self):
        self.seed = os.urandom(32)
        self.public = _pcompress(_pmul(int.from_bytes(hashlib.sha512(self.seed).digest()[:32], "little") & ((1 << 254) - 8) | (1 << 254), _BASE))

    def envelope(self, payload: dict) -> bytes:
        body = json.dumps(payload, separators=(",", ":")).encode()
        signature = sign(self.seed, DOMAIN + body)
        return json.dumps({
            "format": 1,
            "payload": base64.b64encode(body).decode(),
            "signature": base64.b64encode(signature).decode(),
        }, separators=(",", ":")).encode()


def valid_payload(**overrides):
    now = datetime.now(timezone.utc)
    payload = {
        "schema": 1,
        "channel": "stable",
        "version": "9.9.9",
        "seq": 1,
        "previous": None,
        "released_at": now.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "expires_at": (now + timedelta(days=2)).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "min_agent_version": "0.6.25",
        "artifacts": {
            "amd64": {"name": "impreza-agent-linux-amd64", "sha256": "a" * 64, "size": 1234},
        },
    }
    payload.update(overrides)
    return payload


def write_verify_py(directory):
    start = SCRIPT.index("cat > \"$3/verify.py\" <<'PYMANIFEST'")
    body_start = SCRIPT.index("\n", start) + 1
    body_end = SCRIPT.index("\nPYMANIFEST", body_start)
    path = os.path.join(directory, "verify.py")
    with open(path, "w", newline="\n") as handle:
        handle.write(SCRIPT[body_start:body_end] + "\n")
    return path


class InstallManifestVerification(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="install-manifest-")
        self.key = Key()
        self.verify = write_verify_py(self.tmp)
        self.other = Key()

    def run_stages(self, envelope: bytes, state=None, channel="stable"):
        env = dict(os.environ, IMPREZA_RELEASE_CHANNEL=channel, IMPREZA_RELEASE_ARCH="amd64",
                   IMPREZA_RELEASE_PUBLIC_KEY=base64.b64encode(self.key.public).decode())
        paths = [os.path.join(self.tmp, n) for n in ("envelope.json", "state.json", "work")]
        os.makedirs(paths[2], exist_ok=True)
        with open(paths[0], "wb") as h:
            h.write(envelope)
        if state is not None:
            with open(paths[1], "w") as h:
                json.dump(state, h)
        elif os.path.exists(paths[1]):
            os.remove(paths[1])  # a fresh install has NO state file at all
        frame = subprocess.run([sys.executable, self.verify, "frame", *paths], env=env, capture_output=True, text=True)
        if frame.returncode != 0:
            return frame
        work = paths[2]
        # The signature stage exactly as verify_manifest runs it when the
        # host openssl has no raw-Ed25519 path (the built-in RFC 8032 one).
        ed = subprocess.run([sys.executable, self.verify, "ed25519",
                             os.path.join(work, "pub.der"), os.path.join(work, "msg.bin"), os.path.join(work, "sig.bin")],
                            env=env, capture_output=True, text=True)
        if ed.returncode != 0:
            return ed
        return subprocess.run([sys.executable, self.verify, "validate", *paths], env=env, capture_output=True, text=True)

    def test_valid_manifest_passes(self):
        result = self.run_stages(self.key.envelope(valid_payload()))
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_tampered_payload_refused(self):
        envelope = json.loads(self.key.envelope(valid_payload()))
        raw = base64.b64decode(envelope["payload"])
        forged = json.loads(raw)
        forged["version"] = "9.9.10"
        envelope["payload"] = base64.b64encode(json.dumps(forged, separators=(",", ":")).encode()).decode()
        result = self.run_stages(json.dumps(envelope).encode())
        self.assertNotEqual(result.returncode, 0)

    def test_signature_by_other_key_refused(self):
        result = self.run_stages(self.other.envelope(valid_payload()))
        self.assertNotEqual(result.returncode, 0)

    def test_wrong_channel_refused(self):
        result = self.run_stages(self.key.envelope(valid_payload(channel="beta")))
        self.assertNotEqual(result.returncode, 0)

    def test_expired_manifest_refused(self):
        result = self.run_stages(self.key.envelope(valid_payload(expires_at=datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))))
        self.assertNotEqual(result.returncode, 0)

    def test_sequence_rollback_refused(self):
        first = self.key.envelope(valid_payload(seq=7, previous="c" * 64))
        result = self.run_stages(first)
        self.assertEqual(result.returncode, 0, result.stderr)
        with open(os.path.join(self.tmp, "work", "state.candidate.json")) as h:
            state = json.load(h)
        older = self.key.envelope(valid_payload(seq=3, previous="b" * 64))
        result = self.run_stages(older, state=state)
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
