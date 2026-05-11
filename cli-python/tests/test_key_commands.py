"""Unit tests for ``impreza key whoami``."""

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


def _identity_envelope(
    *,
    request_ip: str = "1.2.3.4",
    whitelist: list[dict[str, object]] | None = None,
) -> dict[str, object]:
    return {
        "success": True,
        "meta": {"request_id": "req_t"},
        "data": {
            "id": 21,
            "client_id": 1,
            "prefix": "imp_a1b2c3d4",
            "label": "ci-bot",
            "status": "active",
            "last_used_at": "2026-05-09 18:14:38",
            "created_at": "2026-05-08 16:56:32",
            "rate_limit_per_minute": 60,
            "ip_whitelist": whitelist
            if whitelist is not None
            else [
                {
                    "id": 6,
                    "ip_address": "1.2.3.4",
                    "label": "office",
                    "created_at": "2026-04-01 13:04:40",
                }
            ],
            "request_ip": request_ip,
        },
    }


@respx.mock
def test_whoami_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(200, json=_identity_envelope()),
    )
    result = runner.invoke(app, ["key", "whoami"])
    assert result.exit_code == 0, result.stderr
    # Identity header
    assert "imp_a1b2c3d4" in result.stdout
    assert "ci-bot" in result.stdout
    assert "1.2.3.4" in result.stdout
    # Whitelist sub-table heading
    assert "IP whitelist" in result.stdout


@respx.mock
def test_whoami_marks_current_ip(seeded_config: Path) -> None:
    """The whitelist sub-table includes a `current` boolean column —
    the entry whose ip_address matches request_ip should render as
    "yes" (or the JSON equivalent)."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200,
            json=_identity_envelope(
                request_ip="1.2.3.4",
                whitelist=[
                    {
                        "id": 1,
                        "ip_address": "1.2.3.4",
                        "label": "current",
                        "created_at": "2026-04-01",
                    },
                    {
                        "id": 2,
                        "ip_address": "5.6.7.8",
                        "label": "other",
                        "created_at": "2026-04-02",
                    },
                ],
            ),
        )
    )
    result = runner.invoke(app, ["key", "whoami"])
    assert result.exit_code == 0
    # Both IPs visible
    assert "1.2.3.4" in result.stdout
    assert "5.6.7.8" in result.stdout
    # `yes` for the matching IP, `no` for the other (per ASCII glyphs)
    assert "yes" in result.stdout
    assert "no" in result.stdout


@respx.mock
def test_whoami_empty_whitelist_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200, json=_identity_envelope(whitelist=[])
        )
    )
    result = runner.invoke(app, ["key", "whoami"])
    assert result.exit_code == 0
    assert "no IP whitelist configured" in result.stdout


@respx.mock
def test_whoami_json_emits_full_identity(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(200, json=_identity_envelope()),
    )
    result = runner.invoke(app, ["key", "whoami", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["id"] == 21
    assert parsed["prefix"] == "imp_a1b2c3d4"
    assert isinstance(parsed["ip_whitelist"], list)
    assert parsed["ip_whitelist"][0]["ip_address"] == "1.2.3.4"


@respx.mock
def test_whoami_403_renders_friendly_error(seeded_config: Path) -> None:
    """If the test runner machine isn't on the whitelist, the API
    returns 403 with code=IP_NOT_WHITELISTED before this endpoint
    even runs. Map cleanly to a friendly stderr message."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            403,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {
                    "code": "IP_NOT_WHITELISTED",
                    "message": "Your IP is not on this key's whitelist.",
                },
            },
        )
    )
    result = runner.invoke(app, ["key", "whoami"])
    assert result.exit_code == 1
    assert "Your IP is not on this key's whitelist" in result.stderr
    assert "IP_NOT_WHITELISTED" in result.stderr


def test_whoami_with_no_contexts_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["key", "whoami"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr
