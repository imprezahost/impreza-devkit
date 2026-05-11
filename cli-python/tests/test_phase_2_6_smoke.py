"""Live integration smoke for Phase 2.6 (`impreza invoice *` + `impreza key whoami`).

Skips silently when ``IMPREZA_API_KEY`` / ``IMPREZA_API_SECRET`` are
not set. All commands are read-only.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_2_6_smoke.py -v -s
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


# ── invoice list ────────────────────────────────────────────────────


def test_smoke_invoice_list_returns_typed_rows(
    live_seeded_config: Path,
) -> None:
    """The account has invoices going back to provisioning (the
    test account has 50+ orders per the Phase 1.4d sign-off, so
    this is non-empty)."""
    result = runner.invoke(app, ["invoice", "list", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    for row in parsed[:5]:
        assert isinstance(row["id"], int)
        assert isinstance(row["status"], str)
        assert isinstance(row["total"], (int, float))
        assert isinstance(row["date"], str)

    statuses = sorted({r["status"] for r in parsed})
    print(f"\n  invoices: {len(parsed)} total; statuses: {statuses}")
    for row in parsed[:3]:
        print(
            f"    id={row['id']:>5} {row['status']:<10} "
            f"total={row['total']} date={row['date']}"
        )


def test_smoke_invoice_list_status_filter(live_seeded_config: Path) -> None:
    """Filter for Paid invoices and confirm every returned row has
    that status."""
    result = runner.invoke(
        app, ["invoice", "list", "--status", "Paid", "--output", "json"]
    )
    assert result.exit_code == 0, result.stderr
    parsed = json.loads(result.stdout)
    assert all(r["status"] == "Paid" for r in parsed)
    print(f"\n  paid invoices: {len(parsed)}")


# ── invoice show ────────────────────────────────────────────────────


def test_smoke_invoice_show_round_trips(live_seeded_config: Path) -> None:
    """Pull an id from `invoice list` and show it. Skip if the
    account has no invoices at all."""
    list_result = runner.invoke(app, ["invoice", "list", "--output", "json"])
    parsed = json.loads(list_result.stdout)
    if not parsed:
        pytest.skip("no invoices on this account")

    target = parsed[0]
    result = runner.invoke(
        app,
        ["invoice", "show", str(target["id"]), "--output", "json"],
    )
    assert result.exit_code == 0, result.stderr
    detail = json.loads(result.stdout)
    assert detail["id"] == target["id"]
    assert detail["status"] == target["status"]
    assert isinstance(detail.get("items"), list)
    print(
        f"\n  invoice show {detail['id']}: status={detail['status']!r} "
        f"items={len(detail['items'])} "
        f"transactions={len(detail.get('transactions') or [])}"
    )


# ── key whoami ──────────────────────────────────────────────────────


def test_smoke_key_whoami_returns_identity(live_seeded_config: Path) -> None:
    """The active key always has SOMETHING — at minimum the prefix,
    label, and the request_ip the server observed."""
    result = runner.invoke(app, ["key", "whoami", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed["id"], int)
    assert parsed["prefix"].startswith("imp_")
    assert len(parsed["prefix"]) == 12
    assert parsed["status"] == "active"
    assert isinstance(parsed["rate_limit_per_minute"], int)
    assert isinstance(parsed["request_ip"], str)
    assert isinstance(parsed["ip_whitelist"], list)

    print(
        f"\n  key {parsed['prefix']!r} "
        f"label={parsed['label']!r} status={parsed['status']!r}"
    )
    print(
        f"  request_ip={parsed['request_ip']!r}, "
        f"whitelist=({len(parsed['ip_whitelist'])} entries):"
    )
    for ip in parsed["ip_whitelist"]:
        is_current = ip["ip_address"] == parsed["request_ip"]
        marker = "*" if is_current else " "
        print(
            f"    {marker} {ip['ip_address']:<16} "
            f"{ip['label']!r}"
        )
