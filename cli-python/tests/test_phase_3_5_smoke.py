"""Live integration smoke for Phase 3.5 (VPS Cloud sub-resources).

Bucketed by reversibility, the same way 3.3 / 3.4 are:

* **Safe read-only** (images list, ssh-keys list, vnc read):
  no state changes. Always-safe — but gated behind
  ``IMPREZA_DESTRUCTIVE_TESTS=1`` for symmetry with other 3.x
  smokes so a casual ``pytest`` run never touches the live API.
* **Catastrophic** (images create/restore/delete, resize,
  boot-order, vnc-password rotate, rescue enable, iso mount,
  ipv6 enable): real state changes — some reversible
  (boot-order, rescue, iso) and some that consume storage quota
  (images create) or have billing impact (resize). Mock-only by
  design.

The cheap round-trips that 3.3's set-hostname / 3.4's snapshots
create+delete enabled don't have a direct equivalent here:

  - vnc read returns short-lived credentials; testing them
    would require an actual VNC client. Just exercise the
    request-response.

The earlier ``vps cloud rdns`` smoke (read for the VPS's
dedicated_ip) was removed in 7.7 along with the CLI verbs —
the public-edge WAF rejects /vps/cloud/rdns/{ip} paths and
returns HTML instead of JSON. SDK-level unit tests for the
underlying ``vps.rdns`` resource still live in
sdk-python/tests/test_vps_specific_unit.py.

If we add reversible round-trips later, gate them behind
extra opt-in flags (``IMPREZA_TEST_ALLOW_RDNS_CHANGE=1`` etc.),
following the 3.3 ``IMPREZA_TEST_ALLOW_PASSWORD_RESET`` pattern.

Required env::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    export IMPREZA_TEST_CLOUD_VPS_ID=12345
    export IMPREZA_DESTRUCTIVE_TESTS=1
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest
from typer.testing import CliRunner

from impreza_cli.config import Config
from impreza_cli.main import app

runner = CliRunner()


def _live_creds() -> tuple[str, str]:
    api_key = os.environ.get("IMPREZA_API_KEY", "")
    api_secret = os.environ.get("IMPREZA_API_SECRET", "")
    if not api_key or not api_secret:
        pytest.skip("IMPREZA_API_KEY / IMPREZA_API_SECRET not set")
    return api_key, api_secret


def _test_cloud_vps_id() -> int:
    """A separate var from IMPREZA_TEST_VPS_ID (which the 3.2/3.3/3.4
    smokes use for the Proxmox test VPS). The 3.5 smokes need a
    Cloud-backend VPS — different beast at the Cloud backend layer."""
    raw = os.environ.get("IMPREZA_TEST_CLOUD_VPS_ID", "")
    if not raw:
        pytest.skip("IMPREZA_TEST_CLOUD_VPS_ID not set")
    try:
        return int(raw)
    except ValueError:
        pytest.fail(
            f"IMPREZA_TEST_CLOUD_VPS_ID must be an integer, got: {raw!r}"
        )


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


# ── images: list (read-only) ────────────────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account (gated for symmetry with other 3.x smokes)",
)
def test_smoke_images_list(live_seeded_config: Path) -> None:
    """The image catalog is account-scoped — empty or populated
    are both legitimate. Skipped on Proxmox backend with the
    friendly stderr line."""
    vps_id = _test_cloud_vps_id()
    result = runner.invoke(
        app,
        ["vps", "cloud", "images", "list", str(vps_id), "--output", "json"],
    )
    if result.exit_code != 0:
        if "Proxmox backend" in result.stderr:
            pytest.skip(f"VPS {vps_id} is on Proxmox — images are Cloud-only")
        pytest.fail(f"images list failed: {result.stderr.strip()!r}")
    if result.stdout.strip().startswith("["):
        images = json.loads(result.stdout)
        print(f"\n  Cloud account: {len(images)} saved image(s)")
    else:
        print("\n  Cloud account: no saved images")


# ── ssh-keys: list (read-only) ──────────────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account",
)
def test_smoke_ssh_keys_list(live_seeded_config: Path) -> None:
    """Account-scoped read."""
    vps_id = _test_cloud_vps_id()
    result = runner.invoke(
        app,
        ["vps", "cloud", "ssh-keys", "list", str(vps_id), "--output", "json"],
    )
    if result.exit_code != 0:
        if "Proxmox backend" in result.stderr:
            pytest.skip(f"VPS {vps_id} is on Proxmox — ssh-keys is Cloud-only")
        pytest.fail(f"ssh-keys list failed: {result.stderr.strip()!r}")
    if result.stdout.strip().startswith("["):
        keys = json.loads(result.stdout)
        print(f"\n  Cloud account: {len(keys)} registered SSH key(s)")
    else:
        print("\n  Cloud account: no SSH keys")


# ── vnc: read credentials (no state change) ─────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads VNC credentials from a real VM",
)
def test_smoke_vnc_read(live_seeded_config: Path) -> None:
    """VNC reads the host:port:password triple. No state change —
    just exercises the GET path through the bound model.

    the Cloud backend rejects VNC reads on some VPS configurations (HTTP
    502 wrapped as UPSTREAM_ERROR by our controller) — when that
    happens we skip rather than fail, since the integration plumbing
    is verified by the unit tests with respx mocks.
    """
    vps_id = _test_cloud_vps_id()
    result = runner.invoke(
        app, ["vps", "cloud", "vnc", str(vps_id), "--output", "json"]
    )
    if result.exit_code != 0:
        if "Proxmox backend" in result.stderr:
            pytest.skip(f"VPS {vps_id} is on Proxmox — vnc is Cloud-only")
        if "502" in result.stderr or "UPSTREAM_ERROR" in result.stderr:
            pytest.skip(
                f"Cloud backend rejected VNC read on VPS {vps_id} "
                f"(upstream 502): {result.stderr.strip()!r}"
            )
        pytest.fail(f"vnc read failed: {result.stderr.strip()!r}")
    parsed = json.loads(result.stdout)
    assert isinstance(parsed.get("ip"), str)
    assert isinstance(parsed.get("port"), int)
    print(
        f"\n  VPS {vps_id}: VNC at {parsed['ip']}:{parsed['port']} "
        f"(password len {len(parsed.get('password', ''))})"
    )


# rdns smoke test removed in 7.7. The `vps cloud rdns get` verb is
# no longer exposed by the CLI (WAF-blocked endpoint — the public
# edge returns the maintenance HTML page for /vps/cloud/rdns/{ip}
# paths with dotted-IPv4 segments). Re-add when the server-side WAF
# rule is fixed and `rdns_app` returns to commands/vps_cloud.py.
