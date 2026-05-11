"""Unit tests for ``impreza account`` commands.

Uses respx to mock the SDK's HTTP layer (the SDK uses httpx under
the hood, so respx intercepts cleanly). Each test seeds an isolated
config with a personal context, then asserts on the CliRunner's
exit code, stdout, and stderr.

Live integration smoke for these commands lives in
test_phase_2_2_smoke.py — the unit tests here cover the rendering
paths and error mapping; the smoke confirms the SDK→API plumbing.
"""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest
import respx
from typer.testing import CliRunner

from impreza_cli.config import Config
from impreza_cli.main import app

runner = CliRunner()

BASE = "https://api.imprezahost.com/v1"
_FAKE_KEY = "imp_" + ("a" * 40)
_FAKE_SECRET = "0" * 64


@pytest.fixture
def seeded_config(isolated_config: Path) -> Path:
    """Bootstrap a single 'personal' context so account commands have
    something to authenticate with."""
    cfg = Config.load(isolated_config)
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()
    return isolated_config


def _account_envelope(**overrides: object) -> dict[str, object]:
    """Default ``GET /account`` payload — overridable per-test."""
    data: dict[str, object] = {
        "id": 1,
        "first_name": "Jane",
        "last_name": "Tester",
        "company": None,
        "email": "test@imprezahost.com",
        "balance": 45.32,
        "currency": "USD",
        "registered_at": "2024-01-15",
    }
    data.update(overrides)
    return {"success": True, "data": data, "meta": {"request_id": "req_t"}}


def _services_envelope(services: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"services": services, "total": len(services)},
        "meta": {"request_id": "req_t"},
    }


# ── account info ──────────────────────────────────────────────────────


@respx.mock
def test_info_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope()),
    )
    result = runner.invoke(app, ["account", "info"])
    assert result.exit_code == 0, result.stderr
    assert "Jane Tester" in result.stdout
    assert "test@imprezahost.com" in result.stdout
    assert "45.32 USD" in result.stdout


@respx.mock
def test_info_renders_json(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope()),
    )
    result = runner.invoke(app, ["account", "info", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert parsed["id"] == 1
    assert parsed["email"] == "test@imprezahost.com"
    # JSON output: balance is the raw float, not the formatted string
    assert parsed["balance"] == 45.32
    assert parsed["currency"] == "USD"


@respx.mock
def test_info_includes_company_when_present(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            200,
            json=_account_envelope(company="Impreza Test LLC"),
        ),
    )
    result = runner.invoke(app, ["account", "info"])
    assert result.exit_code == 0
    assert "Impreza Test LLC" in result.stdout


@respx.mock
def test_info_global_output_flag_works(seeded_config: Path) -> None:
    """Passing --output before the subcommand (root callback) should
    also propagate. This validates the ctx.obj wiring."""
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope()),
    )
    result = runner.invoke(app, ["--output", "json", "account", "info"])
    assert result.exit_code == 0, result.stderr
    parsed = json.loads(result.stdout)
    assert parsed["id"] == 1


@respx.mock
def test_info_per_command_output_overrides_global(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope()),
    )
    # Global says json; per-command says table → table wins.
    result = runner.invoke(
        app,
        ["--output", "json", "account", "info", "--output", "table"],
    )
    assert result.exit_code == 0
    # Table output has the formatted balance string with currency
    assert "45.32 USD" in result.stdout
    # And it should NOT be valid JSON (because it's a Rich table)
    with pytest.raises(json.JSONDecodeError):
        json.loads(result.stdout)


# ── account info — error paths ────────────────────────────────────────


def test_info_with_no_contexts_exits_nonzero(isolated_config: Path) -> None:
    """No contexts configured at all → friendly error, no traceback."""
    result = runner.invoke(app, ["account", "info"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr
    # No traceback leaked
    assert "Traceback" not in result.stderr


def test_info_with_unknown_context_override_exits_nonzero(
    seeded_config: Path,
) -> None:
    result = runner.invoke(app, ["--context", "ghost", "account", "info"])
    assert result.exit_code == 1
    assert "does not exist" in result.stderr


@respx.mock
def test_info_api_error_renders_friendly_message(seeded_config: Path) -> None:
    """A 401 from the API should map to a non-zero exit + stderr
    message, not a Python traceback."""
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            401,
            json={
                "success": False,
                "meta": {"request_id": "req_authfail"},
                "error": {
                    "code": "UNAUTHORIZED",
                    "message": "Invalid API credentials.",
                },
            },
        ),
    )
    result = runner.invoke(app, ["account", "info"])
    assert result.exit_code == 1
    assert "Invalid API credentials" in result.stderr
    assert "UNAUTHORIZED" in result.stderr
    assert "req_authfail" in result.stderr


# ── account balance ──────────────────────────────────────────────────


@respx.mock
def test_balance_default_format(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope(balance=42.5)),
    )
    result = runner.invoke(app, ["account", "balance"])
    assert result.exit_code == 0
    assert result.stdout.strip() == "42.50 USD"


@respx.mock
def test_balance_raw_strips_currency(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope(balance=42.5)),
    )
    result = runner.invoke(app, ["account", "balance", "--raw"])
    assert result.exit_code == 0
    assert result.stdout.strip() == "42.50"


@respx.mock
def test_balance_zero_renders_correctly(seeded_config: Path) -> None:
    """Edge case: zero balance shouldn't print as `0` or `0.0`."""
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope(balance=0.0)),
    )
    result = runner.invoke(app, ["account", "balance"])
    assert result.exit_code == 0
    assert result.stdout.strip() == "0.00 USD"


# ── account services ─────────────────────────────────────────────────


@respx.mock
def test_services_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_services_envelope(
                [
                    {
                        "id": 17988,
                        "domain": "testing2.com",
                        "status": "Active",
                        "product": "Hong Kong VPS I",
                        "product_group": "VPS Server Hong Kong",
                        "billing_cycle": "Monthly",
                        "amount": 25.0,
                        "registered_at": "2026-05-09",
                        "next_due": "2026-06-09",
                        "vps_backend": "proxmox",
                    },
                    {
                        "id": 17987,
                        "domain": "testing1.com",
                        "status": "Active",
                        "product": "VPS I",
                        "product_group": "VPS Server",
                        "billing_cycle": "Monthly",
                        "amount": 17.0,
                        "registered_at": "2026-05-09",
                        "next_due": "2026-06-09",
                        "vps_backend": "cloud",
                    },
                ]
            ),
        )
    )
    result = runner.invoke(app, ["account", "services"])
    assert result.exit_code == 0, result.stderr
    # Identifiers (short, never truncated) and backend discriminators
    # are the durable assertions. Product names get squeezed by Rich's
    # auto-width layout when the test "terminal" is narrow, so we
    # don't assert on them in the table-render path — the JSON path
    # below covers content fidelity.
    assert "17988" in result.stdout
    assert "17987" in result.stdout
    assert "proxmox" in result.stdout
    assert "cloud" in result.stdout
    assert "Active" in result.stdout


@respx.mock
def test_services_filter_passes_status_to_api(seeded_config: Path) -> None:
    route = respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(200, json=_services_envelope([])),
    )
    result = runner.invoke(app, ["account", "services", "--status", "Active"])
    assert result.exit_code == 0
    # SDK forwards `status=` as a query string parameter
    url = str(route.calls.last.request.url)
    assert "status=Active" in url


@respx.mock
def test_services_empty_default_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(200, json=_services_envelope([])),
    )
    result = runner.invoke(app, ["account", "services"])
    assert result.exit_code == 0
    assert "No services on this account yet" in result.stdout


@respx.mock
def test_services_empty_with_filter_mentions_filter(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(200, json=_services_envelope([])),
    )
    result = runner.invoke(
        app,
        ["account", "services", "--status", "Cancelled"],
    )
    assert result.exit_code == 0
    assert "No services with status 'Cancelled'" in result.stdout


@respx.mock
def test_services_json_output(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_services_envelope(
                [
                    {
                        "id": 1,
                        "domain": "example.com",
                        "status": "Active",
                        "product": "Test",
                        "product_group": "Test Group",
                        "billing_cycle": "Monthly",
                        "amount": 10.0,
                        "registered_at": "2026-01-01",
                        "next_due": "2026-02-01",
                        "vps_backend": None,
                    }
                ]
            ),
        )
    )
    result = runner.invoke(app, ["account", "services", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    assert parsed[0]["id"] == 1
    # JSON output: amount is the raw number, not the formatted string
    assert parsed[0]["amount"] == 10.0
