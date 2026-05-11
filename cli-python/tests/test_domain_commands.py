"""Unit tests for ``impreza domain`` commands.

respx-mocked HTTP, isolated config (via the ``isolated_config``
fixture from conftest.py), assertions on exit code + stdout/stderr.

Live integration smoke for these commands lives in
test_phase_2_4_smoke.py.
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


# ── envelope helpers ────────────────────────────────────────────────


def _domain_envelope(**overrides: object) -> dict[str, object]:
    data: dict[str, object] = {
        "domain": "example.com",
        "status": "Active",
        "registration_date": "2024-01-15",
        "next_due_date": "2026-01-15",
        "expires_at": "2026-01-15",
        "nameservers": ["ns1.imprezahost.com", "ns2.imprezahost.com"],
        "lock_status": True,
        "id_protection": True,
        "auto_renew": False,
        "epp_code": None,
        "privacy": True,
    }
    data.update(overrides)
    return {"success": True, "data": data, "meta": {"request_id": "req_t"}}


def _check_envelope(availability: dict[str, bool]) -> dict[str, object]:
    """The SDK extractor looks for an ``availability`` key inside
    ``data`` — match that exact shape."""
    return {
        "success": True,
        "data": {"availability": availability},
        "meta": {"request_id": "req_t"},
    }


def _dns_envelope(records: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"records": records, "total": len(records)},
        "meta": {"request_id": "req_t"},
    }


def _tlds_envelope(tlds: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"tlds": tlds, "total": len(tlds)},
        "meta": {"request_id": "req_t"},
    }


# ── domain show ─────────────────────────────────────────────────────


@respx.mock
def test_show_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/example.com").mock(
        return_value=httpx.Response(200, json=_domain_envelope()),
    )
    result = runner.invoke(app, ["domain", "show", "example.com"])
    assert result.exit_code == 0, result.stderr
    assert "example.com" in result.stdout
    assert "Active" in result.stdout
    # Nameservers joined with comma in table mode for readability
    assert "ns1.imprezahost.com, ns2.imprezahost.com" in result.stdout


@respx.mock
def test_show_renders_json_with_nameservers_as_list(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/example.com").mock(
        return_value=httpx.Response(200, json=_domain_envelope()),
    )
    result = runner.invoke(
        app, ["domain", "show", "example.com", "--output", "json"]
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["domain"] == "example.com"
    # JSON keeps nameservers as a list — pipe-friendly for jq
    assert parsed["nameservers"] == [
        "ns1.imprezahost.com",
        "ns2.imprezahost.com",
    ]


@respx.mock
def test_show_404_renders_friendly_error(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/not-mine.com").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "meta": {"request_id": "req_404"},
                "error": {"code": "NOT_FOUND", "message": "Domain not found."},
            },
        ),
    )
    result = runner.invoke(app, ["domain", "show", "not-mine.com"])
    assert result.exit_code == 1
    # Specifically catches ResourceNotFound and gives a clearer message
    # than the generic ApiError handler
    assert "is not registered to this account" in result.stderr
    assert "Traceback" not in result.stderr


# ── domain check ────────────────────────────────────────────────────


@respx.mock
def test_check_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/check").mock(
        return_value=httpx.Response(
            200,
            json=_check_envelope(
                {"example.com": False, "available-domain.io": True}
            ),
        ),
    )
    result = runner.invoke(
        app, ["domain", "check", "example.com", "available-domain.io"]
    )
    assert result.exit_code == 0, result.stderr
    assert "example.com" in result.stdout
    assert "available-domain.io" in result.stdout
    # yes/no rendering (per output.py ASCII glyph rules)
    assert "yes" in result.stdout
    assert "no" in result.stdout


@respx.mock
def test_check_passes_domains_as_csv(seeded_config: Path) -> None:
    route = respx.get(f"{BASE}/domains/check").mock(
        return_value=httpx.Response(
            200, json=_check_envelope({"a.com": True, "b.net": False})
        ),
    )
    result = runner.invoke(app, ["domain", "check", "a.com", "b.net"])
    assert result.exit_code == 0
    url = str(route.calls.last.request.url)
    # The SDK forwards the list as a comma-separated `domains=` param
    assert "domains=" in url


@respx.mock
def test_check_json_preserves_input_order(seeded_config: Path) -> None:
    """Table mode sorts alphabetically; JSON output preserves the
    input order so consumers don't have to re-sort to align with
    their request batches."""
    respx.get(f"{BASE}/domains/check").mock(
        return_value=httpx.Response(
            200,
            json=_check_envelope({"z-domain.io": True, "a-domain.io": False}),
        ),
    )
    result = runner.invoke(
        app,
        ["domain", "check", "z-domain.io", "a-domain.io", "--output", "json"],
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert [r["domain"] for r in parsed] == ["z-domain.io", "a-domain.io"]


# ── domain pricing ──────────────────────────────────────────────────


@respx.mock
def test_pricing_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(
            200,
            json=_tlds_envelope(
                [
                    {
                        "tld": ".com",
                        "register": {"1": 12.99, "2": 25.98},
                        "renew": {"1": 14.30},
                        "currency": "USD",
                        "min_years": 1,
                        "cheapest": 12.99,
                    }
                ]
            ),
        ),
    )
    result = runner.invoke(app, ["domain", "pricing"])
    assert result.exit_code == 0, result.stderr
    assert ".com" in result.stdout
    assert "12.99" in result.stdout


@respx.mock
def test_pricing_filter_passes_to_api(seeded_config: Path) -> None:
    route = respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_envelope([])),
    )
    result = runner.invoke(
        app, ["domain", "pricing", "--filter", ".com,.net"]
    )
    assert result.exit_code == 0
    url = str(route.calls.last.request.url)
    # SDK forwards `filter` as the `tld` query param
    assert "tld=" in url


@respx.mock
def test_pricing_empty_with_filter_mentions_filter(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_envelope([])),
    )
    result = runner.invoke(
        app, ["domain", "pricing", "--filter", ".xyz"]
    )
    assert result.exit_code == 0
    assert "No TLDs match the filter" in result.stdout


# ── domain dns list ─────────────────────────────────────────────────


@respx.mock
def test_dns_list_renders_records(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(
            200,
            json=_dns_envelope(
                [
                    {
                        "type": "A",
                        "host": "@",
                        "value": "1.2.3.4",
                        "ttl": 3600,
                        "priority": None,
                    },
                    {
                        "type": "MX",
                        "host": "@",
                        "value": "mail.example.com",
                        "ttl": 3600,
                        "priority": 10,
                    },
                ]
            ),
        ),
    )
    result = runner.invoke(app, ["domain", "dns", "list", "example.com"])
    assert result.exit_code == 0, result.stderr
    assert "1.2.3.4" in result.stdout
    assert "mail.example.com" in result.stdout
    # Priority shows for MX, dash for A
    assert "10" in result.stdout


@respx.mock
def test_dns_list_empty_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/empty.com/dns").mock(
        return_value=httpx.Response(200, json=_dns_envelope([])),
    )
    result = runner.invoke(app, ["domain", "dns", "list", "empty.com"])
    assert result.exit_code == 0
    assert "No DNS records on 'empty.com' yet" in result.stdout


@respx.mock
def test_dns_list_404_renders_helpful_message(seeded_config: Path) -> None:
    """When DNS management isn't activated yet, the error message
    points the user at the activate command."""
    respx.get(f"{BASE}/domains/inactive.com/dns").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {
                    "code": "NOT_FOUND",
                    "message": "DNS management not activated.",
                },
            },
        ),
    )
    result = runner.invoke(app, ["domain", "dns", "list", "inactive.com"])
    assert result.exit_code == 1
    assert "DNS management is not active" in result.stderr
    assert "activate" in result.stderr.lower()


@respx.mock
def test_dns_list_json_output(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(
            200,
            json=_dns_envelope(
                [
                    {
                        "type": "A",
                        "host": "@",
                        "value": "1.2.3.4",
                        "ttl": 3600,
                        "priority": None,
                    }
                ]
            ),
        ),
    )
    result = runner.invoke(
        app, ["domain", "dns", "list", "example.com", "--output", "json"]
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list) and len(parsed) == 1
    assert parsed[0]["type"] == "A"
    assert parsed[0]["value"] == "1.2.3.4"
    # Null priority should round-trip as JSON null, not a string
    assert parsed[0]["priority"] is None


# ── shared error path ───────────────────────────────────────────────


def test_domain_with_no_contexts_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["domain", "show", "example.com"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr


# ═══════════════════════════════════════════════════════════════════
#                            WRITES (Phase 3.1)
# ═══════════════════════════════════════════════════════════════════


def _account_envelope(balance: float = 100.0) -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": 1,
            "first_name": "Test",
            "last_name": "User",
            "email": "test@example.com",
            "balance": balance,
            "currency": "USD",
            "registered_at": "2024-01-15",
        },
        "meta": {"request_id": "req_t"},
    }


def _registration_envelope(
    domain: str = "newdomain.com",
    amount: float = 12.99,
) -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "order_id": 9001,
            "invoice_id": 12500,
            "domain": domain,
            "years": 1,
            "amount": amount,
            "currency": "USD",
            "status": "Pending",
        },
        "meta": {"request_id": "req_t"},
    }


def _transfer_envelope(
    domain: str = "transferred.com", amount: float = 14.50
) -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "order_id": 9002,
            "invoice_id": 12501,
            "domain": domain,
            "years": 1,
            "amount": amount,
            "currency": "USD",
            "status": "Pending",
        },
        "meta": {"request_id": "req_t"},
    }


def _ok(data: object | None = None) -> dict[str, object]:
    return {
        "success": True,
        "data": data if data is not None else {},
        "meta": {"request_id": "req_t"},
    }


def _tld_envelope_for_register(
    tld: str = ".com", price_1y: float = 12.99
) -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "tlds": [
                {
                    "tld": tld,
                    "register": {"1": price_1y, "2": price_1y * 2},
                    "renew": {"1": price_1y * 1.1},
                    "currency": "USD",
                    "min_years": 1,
                    "cheapest": price_1y,
                }
            ],
            "total": 1,
        },
        "meta": {"request_id": "req_t"},
    }


# ── register ────────────────────────────────────────────────────────


@respx.mock
def test_register_with_yes_skips_prompt(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tld_envelope_for_register())
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope(balance=100.0))
    )
    route = respx.post(f"{BASE}/domains/register").mock(
        return_value=httpx.Response(201, json=_registration_envelope())
    )
    result = runner.invoke(
        app, ["domain", "register", "newdomain.com", "--years", "1", "--yes"]
    )
    assert result.exit_code == 0, result.stderr
    assert "Registered" in result.stdout
    assert "newdomain.com" in result.stdout
    assert "order #9001" in result.stdout
    body = route.calls.last.request.read()
    assert b"newdomain.com" in body


@respx.mock
def test_register_with_nameservers_passes_them_through(
    seeded_config: Path,
) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tld_envelope_for_register())
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope())
    )
    route = respx.post(f"{BASE}/domains/register").mock(
        return_value=httpx.Response(201, json=_registration_envelope())
    )
    result = runner.invoke(
        app,
        [
            "domain", "register", "newdomain.com",
            "--ns", "ns1.example.com",
            "--ns", "ns2.example.com",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    body = route.calls.last.request.read()
    assert b"ns1.example.com" in body
    assert b"ns2.example.com" in body


@respx.mock
def test_register_insufficient_credit_suggests_topup(
    seeded_config: Path,
) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tld_envelope_for_register())
    )
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_account_envelope(balance=1.0))
    )
    respx.post(f"{BASE}/domains/register").mock(
        return_value=httpx.Response(
            402,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {
                    "code": "INSUFFICIENT_CREDIT",
                    "message": "Need $11.99 more.",
                },
            },
        )
    )
    result = runner.invoke(
        app, ["domain", "register", "newdomain.com", "--yes"]
    )
    assert result.exit_code == 1
    assert "Need $11.99 more" in result.stderr
    # The friendly suggestion to top up
    assert "impreza account topup" in result.stderr


def test_register_decline_at_prompt_cancels(seeded_config: Path) -> None:
    """When the user types 'n' at the confirmation, the command
    exits 0 with a Cancelled message, no API call made."""
    with respx.mock:
        # Set up but mark not-called assertion: the prompt should
        # short-circuit before we hit /domains/pricing.
        result = runner.invoke(
            app,
            ["domain", "register", "newdomain.com"],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


# ── transfer ────────────────────────────────────────────────────────


@respx.mock
def test_transfer_with_yes_passes_epp_to_api(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tld_envelope_for_register())
    )
    route = respx.post(f"{BASE}/domains/transfer").mock(
        return_value=httpx.Response(201, json=_transfer_envelope())
    )
    result = runner.invoke(
        app,
        [
            "domain", "transfer", "transferred.com",
            "--epp", "ABC-XYZ-123",
            "--years", "1",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert "Transfer initiated" in result.stdout
    body = route.calls.last.request.read()
    assert b"ABC-XYZ-123" in body


# ── set-nameservers ─────────────────────────────────────────────────


@respx.mock
def test_set_nameservers_two_or_more(seeded_config: Path) -> None:
    route = respx.put(f"{BASE}/domains/example.com/nameservers").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["domain", "set-nameservers", "example.com", "ns1.foo.com", "ns2.foo.com"],
    )
    assert result.exit_code == 0, result.stderr
    body = route.calls.last.request.read()
    assert b"ns1.foo.com" in body
    assert b"ns2.foo.com" in body


def test_set_nameservers_rejects_single(seeded_config: Path) -> None:
    result = runner.invoke(
        app,
        ["domain", "set-nameservers", "example.com", "ns1.foo.com"],
    )
    assert result.exit_code == 1
    assert "at least 2 nameservers" in result.stderr


# ── lock / unlock ───────────────────────────────────────────────────


@respx.mock
def test_lock(seeded_config: Path) -> None:
    respx.post(f"{BASE}/domains/example.com/lock").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(app, ["domain", "lock", "example.com"])
    assert result.exit_code == 0
    assert "Transfer lock enabled" in result.stdout


@respx.mock
def test_unlock_with_yes_returns_epp(seeded_config: Path) -> None:
    respx.delete(f"{BASE}/domains/example.com/lock").mock(
        return_value=httpx.Response(
            200, json=_ok({"epp_code": "EPP-CODE-9911"})
        )
    )
    result = runner.invoke(app, ["domain", "unlock", "example.com", "--yes"])
    assert result.exit_code == 0, result.stderr
    assert "EPP-CODE-9911" in result.stdout


def test_unlock_decline_at_prompt(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app,
            ["domain", "unlock", "example.com"],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


# ── id-protection ───────────────────────────────────────────────────


@respx.mock
def test_id_protection_with_yes(seeded_config: Path) -> None:
    respx.post(f"{BASE}/domains/example.com/id-protection").mock(
        return_value=httpx.Response(
            201, json=_ok({"invoice_id": 99, "amount": 5.0, "currency": "USD"})
        )
    )
    result = runner.invoke(
        app, ["domain", "id-protection", "example.com", "--yes"]
    )
    assert result.exit_code == 0, result.stderr
    # Either the renderer prints the dict or the success line — both are fine
    assert "example.com" in result.stdout


# ── raa-verify / gdpr-auth / transfer-approval ──────────────────────


@respx.mock
def test_raa_verify(seeded_config: Path) -> None:
    respx.post(f"{BASE}/domains/example.com/raa-verify").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(app, ["domain", "raa-verify", "example.com"])
    assert result.exit_code == 0
    assert "RAA verification" in result.stdout


@respx.mock
def test_gdpr_auth(seeded_config: Path) -> None:
    respx.post(f"{BASE}/domains/example.com/gdpr-auth").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(app, ["domain", "gdpr-auth", "example.com"])
    assert result.exit_code == 0
    assert "GDPR" in result.stdout


@respx.mock
def test_transfer_approval(seeded_config: Path) -> None:
    respx.post(f"{BASE}/domains/example.com/transfer-approval").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app, ["domain", "transfer-approval", "example.com"]
    )
    assert result.exit_code == 0
    assert "Transfer approval" in result.stdout


# ── DNS CRUD ────────────────────────────────────────────────────────


@respx.mock
def test_dns_activate(seeded_config: Path) -> None:
    respx.post(f"{BASE}/domains/example.com/dns/activate").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app, ["domain", "dns", "activate", "example.com"]
    )
    assert result.exit_code == 0, result.stderr
    assert "DNS management activated" in result.stdout


@respx.mock
def test_dns_add_a_record(seeded_config: Path) -> None:
    route = respx.post(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        [
            "domain", "dns", "add", "example.com",
            "--type", "A",
            "--name", "@",
            "--value", "1.2.3.4",
            "--ttl", "3600",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert "Added A record" in result.stdout
    body = route.calls.last.request.read()
    assert b"1.2.3.4" in body
    assert b"3600" in body


@respx.mock
def test_dns_add_mx_with_priority(seeded_config: Path) -> None:
    route = respx.post(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        [
            "domain", "dns", "add", "example.com",
            "--type", "MX",
            "--name", "@",
            "--value", "mail.example.com",
            "--priority", "10",
        ],
    )
    assert result.exit_code == 0, result.stderr
    body = route.calls.last.request.read()
    assert b"mail.example.com" in body
    assert b"10" in body


def test_dns_add_invalid_type(seeded_config: Path) -> None:
    """Invalid record type fails fast on the SDK side before any
    HTTP call. The CLI surfaces the SDK's ValueError as a friendly
    error."""
    with respx.mock:
        result = runner.invoke(
            app,
            [
                "domain", "dns", "add", "example.com",
                "--type", "BOGUS",
                "--name", "@",
                "--value", "x",
            ],
        )
    assert result.exit_code == 1
    assert "BOGUS" in result.stderr or "type" in result.stderr.lower()


@respx.mock
def test_dns_update(seeded_config: Path) -> None:
    route = respx.put(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        [
            "domain", "dns", "update", "example.com",
            "--type", "A",
            "--name", "@",
            "--old-value", "1.2.3.4",
            "--new-value", "5.6.7.8",
        ],
    )
    assert result.exit_code == 0, result.stderr
    body = route.calls.last.request.read()
    assert b"1.2.3.4" in body
    assert b"5.6.7.8" in body


@respx.mock
def test_dns_update_404(seeded_config: Path) -> None:
    respx.put(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {
                    "code": "NOT_FOUND",
                    "message": "Record not found.",
                },
            },
        )
    )
    result = runner.invoke(
        app,
        [
            "domain", "dns", "update", "example.com",
            "--type", "A",
            "--name", "@",
            "--old-value", "1.2.3.4",
            "--new-value", "5.6.7.8",
        ],
    )
    assert result.exit_code == 1
    assert "No matching" in result.stderr or "not found" in result.stderr.lower()


@respx.mock
def test_dns_delete_with_yes(seeded_config: Path) -> None:
    route = respx.delete(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        [
            "domain", "dns", "delete", "example.com",
            "--type", "A",
            "--name", "@",
            "--value", "1.2.3.4",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert "Deleted A record" in result.stdout
    assert route.called


def test_dns_delete_decline_at_prompt(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app,
            [
                "domain", "dns", "delete", "example.com",
                "--type", "A",
                "--name", "@",
                "--value", "1.2.3.4",
            ],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout
