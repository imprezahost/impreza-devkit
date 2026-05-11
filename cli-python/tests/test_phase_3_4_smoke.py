"""Live integration smoke for Phase 3.4 (VPS Proxmox sub-resources).

The four sub-resource groups (snapshots, backups, backup schedules,
network) span a wide destructiveness range:

* **Safe round-trips** (snapshots create + delete): take a sentinel
  snapshot, then immediately delete it. Net-neutral. Gated behind
  ``IMPREZA_DESTRUCTIVE_TESTS=1``.
* **Read-only** (snapshots/backups/schedules list): always safe.
  Also gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1`` for symmetry,
  but the calls themselves don't mutate state.
* **Catastrophic** (snapshots rollback, backups restore, backups
  delete on protected backups): mock-only by design. Running them
  live would discard data; the unit tests with respx already pin
  the URL paths, request bodies, and Operation polling.
* **Schedule mutation** (backup-schedules create + delete): real
  upstream cron entry. Could be smoked safely but the test
  account would accumulate fake schedules over time. Kept
  mock-only for now; revisit if customers report scheduling bugs.
* **Backup create**: would consume real storage quota on the test
  VPS and run for minutes. Mock-only.

Required env::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    export IMPREZA_TEST_VPS_ID=12345
    export IMPREZA_DESTRUCTIVE_TESTS=1
"""

from __future__ import annotations

import json
import os
import time
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


def _test_vps_id() -> int:
    raw = os.environ.get("IMPREZA_TEST_VPS_ID", "")
    if not raw:
        pytest.skip("IMPREZA_TEST_VPS_ID not set")
    try:
        return int(raw)
    except ValueError:
        pytest.fail(f"IMPREZA_TEST_VPS_ID must be an integer, got: {raw!r}")


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


# ── snapshots: list (read-only) ─────────────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account (gated for symmetry with other 3.4 smokes)",
)
def test_smoke_snapshots_list(live_seeded_config: Path) -> None:
    """Read-only — succeeds whether the VPS has snapshots or not.
    Skipped on Cloud backend with the friendly stderr line."""
    vps_id = _test_vps_id()
    result = runner.invoke(
        app, ["vps", "proxmox", "snapshots", "list", str(vps_id),
              "--output", "json"]
    )
    if result.exit_code != 0:
        if "Cloud backend" in result.stderr:
            pytest.skip(f"VPS {vps_id} is on Cloud — snapshots are Proxmox-only")
        pytest.fail(f"snapshots list failed: {result.stderr.strip()!r}")
    # Either an empty stdout (no snapshots message on stdout) or a
    # JSON array. Both are legitimate.
    if result.stdout.strip().startswith("["):
        snapshots = json.loads(result.stdout)
        print(f"\n  VPS {vps_id}: {len(snapshots)} existing snapshot(s)")
    else:
        print(f"\n  VPS {vps_id}: no existing snapshots")


# ── snapshots: create + delete round-trip ───────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="creates and immediately deletes a snapshot on a real VPS",
)
def test_smoke_snapshots_create_delete_round_trip(
    live_seeded_config: Path,
) -> None:
    """Take a sentinel snapshot, confirm via list, then delete.

    Net-neutral by design: the sentinel name is unique per run
    (timestamp suffix) so leftover state from a failed previous run
    won't collide. If the delete step fails, the test fails LOUDLY
    so the leftover sentinel doesn't quietly accumulate.
    """
    vps_id = _test_vps_id()
    # Underscore-friendly name — Proxmox restricts to letters / digits
    # / dashes / underscores. No timestamp colons.
    sentinel = f"phase34_smoke_{int(time.time())}"

    print(f"\n  VPS {vps_id}: snapshot sentinel={sentinel!r}")

    # Step 1 — create.
    create_result = runner.invoke(
        app,
        [
            "vps", "proxmox", "snapshots", "create", str(vps_id), sentinel,
            "--description", "phase-3.4 smoke; auto-deleted",
        ],
    )
    if create_result.exit_code != 0:
        if "Cloud backend" in create_result.stderr:
            pytest.skip(f"VPS {vps_id} is on Cloud — snapshots are Proxmox-only")
        pytest.fail(f"create failed: {create_result.stderr.strip()!r}")
    assert sentinel in create_result.stdout
    print(f"    + create OK: {create_result.stdout.strip()}")

    # Step 2 — confirm via list (best-effort; if list doesn't include
    # the sentinel due to upstream caching, that's surprising but
    # not test-failing as long as delete works).
    list_result = runner.invoke(
        app,
        ["vps", "proxmox", "snapshots", "list", str(vps_id), "--output", "json"],
    )
    if list_result.exit_code == 0 and list_result.stdout.strip().startswith("["):
        snapshots = json.loads(list_result.stdout)
        names = {s.get("name") for s in snapshots}
        if sentinel in names:
            print(f"    + list confirms sentinel present ({len(snapshots)} total)")
        else:
            print(
                f"    ! sentinel not in list (got {len(snapshots)} snapshots) "
                "— continuing to delete anyway"
            )

    # Step 3 — delete (cleanup). Run in a finally-ish: pytest.fail
    # below if delete itself fails, but never skip.
    delete_result = runner.invoke(
        app,
        [
            "vps", "proxmox", "snapshots", "delete", str(vps_id), sentinel,
            "--yes",
        ],
    )
    if delete_result.exit_code != 0:
        pytest.fail(
            f"delete failed: {delete_result.stderr.strip()!r}. "
            f"Sentinel snapshot {sentinel!r} may still exist on VPS {vps_id}."
        )
    print(f"    + delete OK: {delete_result.stdout.strip()}")


# ── backups: list (read-only) ───────────────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account",
)
def test_smoke_backups_list(live_seeded_config: Path) -> None:
    vps_id = _test_vps_id()
    result = runner.invoke(
        app, ["vps", "proxmox", "backups", "list", str(vps_id),
              "--output", "json"]
    )
    if result.exit_code != 0:
        if "Cloud backend" in result.stderr:
            pytest.skip(f"VPS {vps_id} is on Cloud — backups are Proxmox-only")
        pytest.fail(f"backups list failed: {result.stderr.strip()!r}")
    if result.stdout.strip().startswith("["):
        backups = json.loads(result.stdout)
        print(f"\n  VPS {vps_id}: {len(backups)} existing backup(s)")
    else:
        print(f"\n  VPS {vps_id}: no existing backups")


# ── backup-schedules: list (read-only) ──────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account",
)
def test_smoke_backup_schedules_list(live_seeded_config: Path) -> None:
    vps_id = _test_vps_id()
    result = runner.invoke(
        app, ["vps", "proxmox", "backup-schedules", "list", str(vps_id),
              "--output", "json"]
    )
    if result.exit_code != 0:
        if "Cloud backend" in result.stderr:
            pytest.skip(
                f"VPS {vps_id} is on Cloud — backup-schedules are Proxmox-only"
            )
        pytest.fail(f"backup-schedules list failed: {result.stderr.strip()!r}")
    if result.stdout.strip().startswith("["):
        schedules = json.loads(result.stdout)
        print(f"\n  VPS {vps_id}: {len(schedules)} existing schedule(s)")
    else:
        print(f"\n  VPS {vps_id}: no existing schedules")
