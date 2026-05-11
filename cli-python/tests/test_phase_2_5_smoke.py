"""Live integration smoke for Phase 2.5 (`impreza vps *`).

Skips silently when ``IMPREZA_API_KEY`` / ``IMPREZA_API_SECRET`` are
not set. All commands are read-only.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_2_5_smoke.py -v -s
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


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


def test_smoke_vps_list_returns_typed_rows(live_seeded_config: Path) -> None:
    """``impreza vps list`` should return rows for every VPS service
    on the account, regardless of backend. Empty result is acceptable
    on a fresh test account."""
    result = runner.invoke(app, ["vps", "list", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    for row in parsed:
        assert isinstance(row["id"], int)
        assert isinstance(row["domain"], str)
        assert row["backend"] in ("proxmox", "cloud")
        assert isinstance(row["status"], str)

    backends = sorted({r["backend"] for r in parsed})
    print(f"\n  vps list: {len(parsed)} total; backends: {backends}")
    for row in parsed[:5]:
        print(
            f"    id={row['id']:>5} "
            f"{row['backend']:<8} "
            f"{row['status']:<12} "
            f"{row['domain']}"
        )


def test_smoke_vps_list_filter_by_backend(live_seeded_config: Path) -> None:
    """The --backend filter should narrow to a single backend's VPSes
    (or an empty list if the account has none of that kind)."""
    result = runner.invoke(
        app, ["vps", "list", "--backend", "proxmox", "--output", "json"]
    )
    assert result.exit_code == 0, result.stderr
    parsed = json.loads(result.stdout)
    assert all(row["backend"] == "proxmox" for row in parsed)
    print(f"\n  proxmox-only: {len(parsed)}")


def test_smoke_vps_show_round_trips(live_seeded_config: Path) -> None:
    """Pull a service id from `vps list` and show it. Skip if the
    account has no VPS at all."""
    list_result = runner.invoke(app, ["vps", "list", "--output", "json"])
    parsed = json.loads(list_result.stdout)
    if not parsed:
        pytest.skip("no VPS services on this account")

    target = parsed[0]
    result = runner.invoke(
        app, ["vps", "show", str(target["id"]), "--output", "json"]
    )
    assert result.exit_code == 0, result.stderr
    detail = json.loads(result.stdout)
    assert detail["id"] == target["id"]
    assert detail["backend"] == target["backend"]
    print(
        f"\n  vps show {detail['id']}: "
        f"{detail['backend']!r} {detail['status']!r} "
        f"{detail['product']!r}"
    )


def test_smoke_vps_status_returns_power_state(live_seeded_config: Path) -> None:
    """Power state must come back populated for every VPS — Proxmox
    + Cloud both surface it after the 1.7 server normalisation
    fixes."""
    list_result = runner.invoke(app, ["vps", "list", "--output", "json"])
    parsed = json.loads(list_result.stdout)
    if not parsed:
        pytest.skip("no VPS services on this account")

    print(f"\n  status across {len(parsed)} VPS:")
    for vps in parsed:
        result = runner.invoke(
            app, ["vps", "status", str(vps["id"]), "--output", "json"]
        )
        assert result.exit_code == 0, (
            f"vps status {vps['id']} failed: stderr={result.stderr}"
        )
        st = json.loads(result.stdout)
        assert isinstance(st["power_state"], str)
        assert st["power_state"]  # non-empty
        if vps["backend"] == "proxmox":
            # Proxmox should report runtime metrics
            assert isinstance(st["memory_total"], int)
            assert st["memory_total"] > 0
            assert isinstance(st["uptime"], int)
        # Cloud may have None for memory / uptime — that's expected.
        memory_str = (
            f", uptime={st['uptime']}s"
            if st.get("uptime") is not None
            else ""
        )
        print(
            f"    {vps['id']:>5} {vps['backend']:<8} "
            f"power={st['power_state']!r}{memory_str}"
        )
