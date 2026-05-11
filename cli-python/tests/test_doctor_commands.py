"""Unit tests for ``impreza doctor`` (Phase 4.1).

Each check has at least one happy-path test and one failure test.
The doctor renders ``[OK] / [FAIL] / [WARN] / [SKIP]`` labels to
stdout; tests assert against those plus the per-check summary
text. Failure paths verify the exit code is 1 even when the report
itself ran to completion.

The ``--output json`` mode emits a structured array that
monitoring scripts can pipe through ``jq``. JSON tests assert the
shape: ``[{name, ok, summary, detail}, ...]``.
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


def _ok(payload: dict[str, object]) -> dict[str, object]:
    return {
        "success": True,
        "data": payload,
        "meta": {"request_id": "req_test"},
    }


def _key_identity_payload(
    *,
    prefix: str = "imp_aaaaaaaa",
    label: str | None = "ci-bot",
    status: str = "active",
    rate_limit: int = 60,
    request_ip: str = "200.1.2.3",
    whitelist: list[dict[str, object]] | None = None,
) -> dict[str, object]:
    return {
        "id": 1,
        "client_id": 100,
        "prefix": prefix,
        "label": label,
        "status": status,
        "last_used_at": "2026-05-11T10:00:00Z",
        "created_at": "2026-05-01T10:00:00Z",
        "rate_limit_per_minute": rate_limit,
        "ip_whitelist": whitelist if whitelist is not None else [
            {
                "id": 1, "ip_address": "200.1.2.3",
                "label": "home office",
                "created_at": "2026-05-01T10:00:00Z",
            },
        ],
        "request_ip": request_ip,
    }


def _account_payload(
    *,
    first_name: str = "Jane",
    last_name: str = "Doe",
    company: str | None = None,
    email: str = "jane@example.com",
    balance: float = 5.00,
    currency: str = "USD",
) -> dict[str, object]:
    return {
        "id": 100,
        "first_name": first_name,
        "last_name": last_name,
        "company": company,
        "email": email,
        "balance": balance,
        "currency": currency,
        "registered_at": "2024-01-15",
    }


# ── happy path ──────────────────────────────────────────────────────


@respx.mock
def test_doctor_all_green(seeded_config: Path) -> None:
    """Every check passes — report ends with 'All checks passed.
    5/5.' and exit 0."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(200, json=_ok(_key_identity_payload()))
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok(_account_payload()))
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 0, result.stderr
    # All five checks rendered
    assert "active-context" in result.stdout
    assert "api-reachable" in result.stdout
    assert "key-status" in result.stdout
    assert "ip-whitelist" in result.stdout
    assert "account-profile" in result.stdout
    # No FAIL labels
    assert "[FAIL]" not in result.stdout
    # Summary line
    assert "All checks passed" in result.stdout
    # Account info rendered
    assert "Jane Doe" in result.stdout
    assert "5.00 USD" in result.stdout
    # IP match rendered
    assert "200.1.2.3" in result.stdout
    assert "home office" in result.stdout


@respx.mock
def test_doctor_json_output(seeded_config: Path) -> None:
    """JSON mode emits the structured array with the four expected
    keys per entry."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(200, json=_ok(_key_identity_payload()))
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok(_account_payload()))
    )
    result = runner.invoke(app, ["doctor", "--output", "json"])
    assert result.exit_code == 0, result.stderr
    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list) and len(parsed) == 5
    for entry in parsed:
        assert set(entry.keys()) == {"name", "ok", "summary", "detail"}
        assert entry["ok"] is True


# ── api unreachable / network failure ───────────────────────────────


@respx.mock
def test_doctor_network_error_fails_cleanly(seeded_config: Path) -> None:
    """NetworkError from the SDK surfaces as a FAIL on api-reachable;
    subsequent checks skip; exit code is 1."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        side_effect=httpx.ConnectError("connection refused")
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 1
    assert "[FAIL]" in result.stdout
    # api-reachable failed
    assert "Could not reach api.imprezahost.com" in result.stdout
    # Dependent checks skipped
    assert "[SKIP]" in result.stdout
    # Summary now goes through error() helper which routes to stderr
    # with an "Error: " prefix (4.3 palette consistency).
    assert "Error:" in result.stderr
    assert "checks failed" in result.stderr


# ── auth error ──────────────────────────────────────────────────────


@respx.mock
def test_doctor_auth_error(seeded_config: Path) -> None:
    """401 from the API surfaces as 'Authentication failed' with a
    rotate-key hint."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            401,
            json={
                "success": False,
                "meta": {"request_id": "req_test"},
                "error": {"code": "UNAUTHORIZED", "message": "Invalid key."},
            },
        )
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 1
    assert "Authentication failed" in result.stdout
    assert "Rotate via Impreza Account" in result.stdout


# ── IP not whitelisted ──────────────────────────────────────────────


@respx.mock
def test_doctor_ip_not_whitelisted_403(seeded_config: Path) -> None:
    """The server itself returned IP_NOT_WHITELISTED — caught by
    the dedicated except clause, not the generic 403 path."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            403,
            json={
                "success": False,
                "meta": {"request_id": "req_test"},
                "error": {
                    "code": "IP_NOT_WHITELISTED",
                    "message": "IP 1.2.3.4 not whitelisted on this key.",
                },
            },
        )
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 1
    assert "IP not whitelisted" in result.stdout
    assert "Impreza Account" in result.stdout


# ── key paused / revoked ────────────────────────────────────────────


@respx.mock
def test_doctor_key_status_paused(seeded_config: Path) -> None:
    """Key reached and authenticated, but the status field is not
    'active'. Doctor reports FAIL with a rotate hint; other checks
    that depend on the key still run."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_key_identity_payload(status="paused")),
        )
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok(_account_payload()))
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 1
    # api-reachable passed, key-status failed
    assert "status='paused'" in result.stdout
    assert "not active" in result.stdout


# ── IP whitelist mismatch ───────────────────────────────────────────


@respx.mock
def test_doctor_ip_whitelist_mismatch(seeded_config: Path) -> None:
    """The call somehow succeeded (perhaps via a different layer)
    but request_ip doesn't match any whitelist entry — flag as a
    FAIL because future calls may stop working."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_key_identity_payload(
                request_ip="9.9.9.9",
                whitelist=[
                    {"id": 1, "ip_address": "200.1.2.3",
                     "label": "home office",
                     "created_at": "2026-05-01T10:00:00Z"},
                ],
            )),
        )
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok(_account_payload()))
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 1
    assert "request_ip 9.9.9.9 not in whitelist" in result.stdout
    assert "Impreza Account" in result.stdout


@respx.mock
def test_doctor_ip_whitelist_match_with_label(seeded_config: Path) -> None:
    """Happy whitelist case shows the matched entry's label."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_key_identity_payload(
                request_ip="10.20.30.40",
                whitelist=[
                    {"id": 1, "ip_address": "10.20.30.40",
                     "label": "ci-runner",
                     "created_at": "2026-05-01T10:00:00Z"},
                ],
            )),
        )
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok(_account_payload()))
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 0, result.stderr
    assert "matches entry ('ci-runner')" in result.stdout


# ── empty whitelist warning ─────────────────────────────────────────


@respx.mock
def test_doctor_empty_whitelist_warns(seeded_config: Path) -> None:
    """Whitelist empty + call succeeded → WARN not FAIL. Exit 0
    (warnings don't break the contract)."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_key_identity_payload(whitelist=[])),
        )
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok(_account_payload()))
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 0, result.stderr
    assert "[WARN]" in result.stdout
    assert "no whitelist entries" in result.stdout


# ── account profile failure (key works for api_key_self but not /account) ──


@respx.mock
def test_doctor_account_profile_fails(seeded_config: Path) -> None:
    """api-reachable passed, but /account returned 403 (scope-
    restricted key). Doctor catches this and reports the cause."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(200, json=_ok(_key_identity_payload()))
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            403,
            json={
                "success": False,
                "meta": {"request_id": "req_test"},
                "error": {
                    "code": "FORBIDDEN",
                    "message": "Key lacks read scope.",
                },
            },
        )
    )
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 1
    assert "GET /account failed" in result.stdout
    assert "Key lacks read scope" in result.stdout


# ── no contexts configured ──────────────────────────────────────────


def test_doctor_no_contexts(isolated_config: Path) -> None:
    """The fresh-install case: no contexts in the config file. The
    make_client_or_exit helper handles this with its own friendly
    stderr — doctor never gets to run any checks."""
    result = runner.invoke(app, ["doctor"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr


# ── context override ────────────────────────────────────────────────


@respx.mock
def test_doctor_context_override(seeded_config: Path) -> None:
    """``impreza --context X doctor`` reports the override in the
    active-context summary."""
    # Seed a second context.
    cfg = Config.load(seeded_config)
    cfg.add_context("staging", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()

    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(200, json=_ok(_key_identity_payload()))
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok(_account_payload()))
    )
    result = runner.invoke(app, ["--context", "staging", "doctor"])
    assert result.exit_code == 0, result.stderr
    assert "Context override: 'staging'" in result.stdout


# ── JSON exit code on failure ───────────────────────────────────────


@respx.mock
def test_doctor_json_exit_1_on_failure(seeded_config: Path) -> None:
    """JSON mode emits the array AND exits 1 when any check failed —
    monitoring scripts care about the exit code as the primary
    signal."""
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            401,
            json={
                "success": False,
                "meta": {"request_id": "r"},
                "error": {"code": "UNAUTHORIZED", "message": "Invalid key."},
            },
        )
    )
    result = runner.invoke(app, ["doctor", "--output", "json"])
    assert result.exit_code == 1
    parsed = json.loads(result.stdout)
    assert any(entry["ok"] is False for entry in parsed)
