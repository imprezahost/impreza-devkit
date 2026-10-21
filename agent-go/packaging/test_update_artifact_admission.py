"""Userland regression for real updater refusals; no system installation.

Only installation paths and OS/service discovery are isolated. The updater's
actual artifact digest/version checks execute through /bin/sh. Signed-channel integration is covered separately from this isolated fixture.
"""
import argparse
import hashlib
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest

parser = argparse.ArgumentParser(add_help=False)
parser.add_argument("--updater", type=Path, default=Path(__file__).with_name("update.sh"))
options, remaining = parser.parse_known_args()
SOURCE = options.updater.read_text(encoding="utf-8")


@unittest.skipUnless(os.name == "posix" and shutil.which("sh"), "POSIX shell fixture")
class ArtifactAdmission(unittest.TestCase):
    def refusal(self, bad_digest=False, bad_version=False):
        with tempfile.TemporaryDirectory(prefix="impreza-updater-admission-") as raw:
            root = Path(raw)
            binary_dir = root / "installed-bin"
            binary_dir.mkdir()
            binary = binary_dir / "agent"
            unit = root / "ingress.service"
            state = root / "state"
            state.mkdir()
            config = root / "config.toml"
            config.write_text('agent_id="synthetic-userland-only"\n')
            unit.write_bytes(b"existing boot contract must survive refusal\n")
            binary.write_text("#!/bin/sh\necho 'impreza-agent version 0.6.34'\n")
            binary.chmod(0o700)
            tools = root / "tools"
            tools.mkdir()
            events = root / "service-events"
            stubs = {
                "id": "#!/bin/sh\necho 0\n",
                "uname": "#!/bin/sh\ncase $1 in -s) echo Linux;; -m) echo x86_64;; *) exit 1;; esac\n",
                "systemctl": "#!/bin/sh\ncase $1 in cat) exit 0;; show) echo '/usr/local/bin/impreza-agent run';; *) printf '%s\\n' \"$*\" >> " + shlex.quote(str(events)) + ";; esac\n",
            }
            for name, data in stubs.items():
                path = tools / name
                path.write_text(data)
                path.chmod(0o700)
            source = SOURCE
            replacements = {
                "BIN=/usr/local/bin/impreza-agent": "BIN=" + shlex.quote(str(binary)),
                "/etc/impreza-agent/config.toml": str(config),
                "/etc/systemd/system/impreza-agent-ingress.service": str(unit),
                "/run/lock/impreza-agent-update.lock": str(root / "update.lock"),
                "/var/lib/impreza-agent": str(state),
                "/usr/local/bin/.impreza-agent-update.XXXXXX": str(binary_dir / ".update.XXXXXX"),
            }
            for old, new in replacements.items():
                self.assertIn(old, source, "fixture installation path needs review")
                source = source.replace(old, new)
            isolated = root / "update.sh"
            isolated.write_text(source)
            release = root / "release"
            offered = release / "stable" / "0.6.35"
            offered.mkdir(parents=True)
            candidate = offered / "impreza-agent-linux-amd64"
            version = "0.6.36" if bad_version else "0.6.35"
            candidate.write_text("#!/bin/sh\necho 'impreza-agent version " + version + "'\n")
            digest = "0" * 64 if bad_digest else hashlib.sha256(candidate.read_bytes()).hexdigest()
            candidate.with_name(candidate.name + ".sha256").write_text(digest + "\n")
            protected = (binary, config, unit)
            before = {path: path.read_bytes() for path in protected}
            env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"], IMPREZA_AGENT_RELEASE_BASE=str(release), IMPREZA_AGENT_VERSION="0.6.35", IMPREZA_AGENT_CHANNEL="stable", IMPREZA_AGENT_REQUIRE_MANIFEST="0")
            for key in ("IMPREZA_AGENT_SOCKS_PROXY", "IMPREZA_AGENT_EXPECTED_VERSION", "IMPREZA_RELEASE_PUBLIC_KEY"):
                env.pop(key, None)
            result = subprocess.run(["sh", str(isolated), "--apply"], env=env, capture_output=True, text=True, timeout=20)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("Checksum mismatch" if bad_digest else "Release version mismatch", result.stderr)
            self.assertEqual({path: path.read_bytes() for path in protected}, before, "refused artifact changed an installed file")
            self.assertFalse(events.exists(), "refused artifact invoked a mutating service operation")
            self.assertEqual(list(state.iterdir()), [], "refused artifact wrote persistent update state")

    def test_wrong_digest_has_no_installed_side_effect(self):
        self.refusal(bad_digest=True)

    def test_wrong_embedded_version_has_no_installed_side_effect(self):
        self.refusal(bad_version=True)


if __name__ == "__main__":
    unittest.main(argv=[__file__, *remaining])