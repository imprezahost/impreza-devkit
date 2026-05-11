"""Unit tests for ``impreza service cancel`` (Phase 3.7).

Mirrors the cancel-half of ``test_vps_commands.py`` from 3.3 but
exercises the non-backend-specific surface (which the SDK shipped
in 3.7 via ``c.account.services.cancel``).
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
    cfg = Config.load(isolated_config)
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()
    return isolated_config


def _ok(payload: dict[str, object] | None = None) -> dict[str, object]:
    return {
        "success": True,
        "data": payload if payload is not None else {},
        "meta": {"request_id": "req_t"},
    }


@respx.mock
def test_cancel_defaults_to_end_of_billing(seeded_config: Path) -> None:
    """Default --type is 'End of Billing Period' — protect prepaid
    days from accidental immediate cancellation."""
    route = respx.post(f"{BASE}/services/15957/cancel").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app, ["service", "cancel", "15957", "--yes"]
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"type": "End of Billing Period"}
    assert "Cancellation submitted for service 15957" in result.stdout


@respx.mock
def test_cancel_immediate_with_reason(seeded_config: Path) -> None:
    """--type Immediate + --reason sends both in the body."""
    route = respx.post(f"{BASE}/services/15957/cancel").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        [
            "service", "cancel", "15957",
            "--type", "Immediate",
            "--reason", "moving providers",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"type": "Immediate", "reason": "moving providers"}


def test_cancel_invalid_type(seeded_config: Path) -> None:
    """Client-side validation rejects unknown --type before HTTP."""
    result = runner.invoke(
        app,
        ["service", "cancel", "15957", "--type", "Whenever", "--yes"],
    )
    assert result.exit_code == 1
    assert "--type must be one of" in result.stderr


def test_cancel_decline_at_prompt(seeded_config: Path) -> None:
    """Typing n at the prompt exits 0 with Cancelled — no HTTP."""
    with respx.mock:
        result = runner.invoke(
            app, ["service", "cancel", "15957"], input="n\n"
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


@respx.mock
def test_cancel_404_renders_friendly_error(seeded_config: Path) -> None:
    """Unknown service id surfaces the standard ApiError stderr line."""
    respx.post(f"{BASE}/services/9999/cancel").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {"code": "NOT_FOUND", "message": "Service not found."},
            },
        )
    )
    result = runner.invoke(
        app, ["service", "cancel", "9999", "--yes"]
    )
    assert result.exit_code == 1
    assert "Service not found" in result.stderr
    assert "Traceback" not in result.stderr


@respx.mock
def test_cancel_403_when_not_owned(seeded_config: Path) -> None:
    """Server-side ownership check (Auth::ownsService) returns 403
    when the client tries to cancel someone else's service.
    The CLI passes through the friendly message."""
    respx.post(f"{BASE}/services/12345/cancel").mock(
        return_value=httpx.Response(
            403,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {
                    "code": "FORBIDDEN",
                    "message": "You do not own this service.",
                },
            },
        )
    )
    result = runner.invoke(
        app, ["service", "cancel", "12345", "--yes"]
    )
    assert result.exit_code == 1
    assert "do not own this service" in result.stderr
