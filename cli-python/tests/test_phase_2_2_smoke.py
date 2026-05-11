"""Live integration smoke for Phase 2.2 (`impreza account *`).

Skips silently when ``IMPREZA_API_KEY`` / ``IMPREZA_API_SECRET`` are
not in the env, mirroring the SDK's smoke pattern. The CLI lives in
the same world as the SDK — what works for the SDK lives here too,
just exercised through the Typer entry point.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_2_2_smoke.py -v -s

These tests don't touch the user's real config: the
``isolated_config`` fixture (see ``conftest.py``) sets
``IMPREZA_CONFIG`` to a per-test temp file, and we seed a context
inside the test from the env-var creds.
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
    """Seed an isolated config with the live creds. Tests that
    request this fixture skip when no creds are configured."""
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


def test_smoke_account_info_renders_balance(live_seeded_config: Path) -> None:
    """``impreza account info`` should hit /account and render the
    real client profile."""
    result = runner.invoke(app, ["account", "info", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed["id"], int) and parsed["id"] > 0
    assert "@" in parsed["email"]
    assert isinstance(parsed["balance"], (int, float))
    assert isinstance(parsed["currency"], str) and parsed["currency"]
    print(
        f"\n  account id={parsed['id']} email={parsed['email']} "
        f"balance={parsed['balance']} {parsed['currency']}"
    )


def test_smoke_account_balance_default_format(live_seeded_config: Path) -> None:
    result = runner.invoke(app, ["account", "balance"])
    assert result.exit_code == 0, result.stderr
    line = result.stdout.strip()
    # Format: "<float> <ISO-currency-code>"
    parts = line.split()
    assert len(parts) == 2
    float(parts[0])
    assert parts[1].isalpha() and len(parts[1]) == 3
    print(f"\n  balance default: {line}")


def test_smoke_account_balance_raw_is_just_number(
    live_seeded_config: Path,
) -> None:
    result = runner.invoke(app, ["account", "balance", "--raw"])
    assert result.exit_code == 0, result.stderr
    line = result.stdout.strip()
    # Must parse as a single float — useful for shell substitutions
    float(line)
    print(f"\n  balance raw:     {line}")


def test_smoke_account_services_returns_typed_rows(
    live_seeded_config: Path,
) -> None:
    """``impreza account services`` should return rows shaped like
    Service models. We don't assert on count — empty is acceptable
    for a fresh test account."""
    result = runner.invoke(app, ["account", "services", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    for row in parsed:
        assert isinstance(row["id"], int)
        assert isinstance(row["domain"], str)
        assert isinstance(row["status"], str)
        # vps_backend may be None for non-VPS services; when present
        # it must be one of the documented discriminator values.
        if row["vps_backend"] is not None:
            assert row["vps_backend"] in ("proxmox", "cloud")
    print(f"\n  services: {len(parsed)} total")
    for row in parsed[:3]:
        print(
            f"    id={row['id']:>5} {row['status']:<12} "
            f"{row['vps_backend'] or '-':<8} {row['domain']}"
        )
    if len(parsed) > 3:
        print(f"    ... and {len(parsed) - 3} more")


def test_smoke_account_services_status_filter_round_trips(
    live_seeded_config: Path,
) -> None:
    """Filter for Active services and verify every returned row has
    that status — confirms the SDK forwards `status=` to the API."""
    result = runner.invoke(
        app,
        ["account", "services", "--status", "Active", "--output", "json"],
    )
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    for row in parsed:
        assert row["status"] == "Active"
    print(f"\n  active services: {len(parsed)}")
