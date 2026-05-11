"""Unit tests for the email resource (Phase 1.4c).

Covers Titan (3 ops) and Google Workspace (3 ops), sync + async,
mocked via respx — no real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import AsyncClient, Client, TitanSsoUrl

BASE = "https://api.imprezahost.com/v1"


def _ok(data: dict[str, object]) -> dict[str, object]:
    return {
        "success": True,
        "data": data,
        "meta": {"request_id": "req_test"},
    }


# ── Titan: sync ────────────────────────────────────────────────────────


@respx.mock
def test_titan_get_returns_order_details_dict() -> None:
    respx.get(f"{BASE}/email/titan/example.com").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "orderid": 12345,
                    "domain": "example.com",
                    "plan": "Professional",
                    "accounts_used": 5,
                    "accounts_total": 10,
                    "expires_at": "2027-01-01",
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        details = c.email.titan.get("example.com")
    assert details["plan"] == "Professional"
    assert details["accounts_used"] == 5


@respx.mock
def test_titan_dns_records_extracts_list() -> None:
    respx.get(f"{BASE}/email/titan/example.com/dns").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "dns_records": [
                        {"type": "MX", "host": "@", "value": "mx1.titan.email", "priority": 10},
                        {"type": "TXT", "host": "@", "value": "v=spf1 include:..."},
                    ]
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        records = c.email.titan.dns_records("example.com")
    assert len(records) == 2
    assert records[0]["type"] == "MX"
    assert records[1]["type"] == "TXT"


@respx.mock
def test_titan_dns_records_empty_on_missing_key() -> None:
    respx.get(f"{BASE}/email/titan/example.com/dns").mock(
        return_value=httpx.Response(200, json=_ok({}))
    )
    with Client(api_key="x", api_secret="y") as c:
        assert c.email.titan.dns_records("example.com") == []


@respx.mock
def test_titan_sso_returns_typed_model() -> None:
    respx.get(f"{BASE}/email/titan/example.com/sso").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"sso_url": "https://titan.email/sso/abc", "iframe_url": "https://titan.email/iframe/abc"}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        sso = c.email.titan.sso("example.com")
    assert isinstance(sso, TitanSsoUrl)
    assert sso.sso_url.startswith("https://titan.email/sso/")
    assert sso.iframe_url is not None


@respx.mock
def test_titan_sso_iframe_optional() -> None:
    respx.get(f"{BASE}/email/titan/example.com/sso").mock(
        return_value=httpx.Response(200, json=_ok({"sso_url": "https://titan.email/sso/x"}))
    )
    with Client(api_key="x", api_secret="y") as c:
        sso = c.email.titan.sso("example.com")
    assert sso.iframe_url is None


# ── Google Workspace: sync ─────────────────────────────────────────────


@respx.mock
def test_google_get_returns_order_details_dict() -> None:
    respx.get(f"{BASE}/email/google/fingerdrones.com.br").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "orderid": 67890,
                    "domain": "fingerdrones.com.br",
                    "plan": "Business Standard",
                    "seats_used": 3,
                    "seats_total": 10,
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        details = c.email.google.get("fingerdrones.com.br")
    assert details["plan"] == "Business Standard"
    assert details["seats_used"] == 3


@respx.mock
def test_google_dns_records_no_domain_segment() -> None:
    """Google DNS records are account-level — no domain in the path."""
    route = respx.get(f"{BASE}/email/google/dns").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "dns_records": [
                        {"type": "MX", "host": "@", "value": "smtp.google.com", "priority": 1},
                    ]
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        records = c.email.google.dns_records()
    assert route.called
    assert len(records) == 1
    assert records[0]["value"] == "smtp.google.com"


@respx.mock
def test_google_setup_admin_posts_full_body() -> None:
    route = respx.post(f"{BASE}/email/google/example.com/admin").mock(
        return_value=httpx.Response(
            201, json=_ok({"admin": "admin@example.com", "status": "created"})
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        result = c.email.google.setup_admin(
            "example.com",
            email_address="admin@example.com",
            first_name="John",
            last_name="Doe",
            alternate_email="john@gmail.com",
            name="John Doe",
            company="ACME",
            zip="12345",
        )
    assert route.called
    body = route.calls.last.request.read()
    for needle in (b"admin@example.com", b"John", b"Doe", b"ACME", b"12345"):
        assert needle in body, f"missing {needle!r} in body"
    assert result["status"] == "created"


# ── async ──────────────────────────────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_titan_get() -> None:
    respx.get(f"{BASE}/email/titan/example.com").mock(
        return_value=httpx.Response(200, json=_ok({"plan": "Pro"}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        details = await c.email.titan.get("example.com")
    assert details["plan"] == "Pro"


@pytest.mark.asyncio
@respx.mock
async def test_async_titan_sso_typed() -> None:
    respx.get(f"{BASE}/email/titan/example.com/sso").mock(
        return_value=httpx.Response(200, json=_ok({"sso_url": "https://titan.email/sso/y"}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        sso = await c.email.titan.sso("example.com")
    assert isinstance(sso, TitanSsoUrl)
    assert sso.sso_url == "https://titan.email/sso/y"


@pytest.mark.asyncio
@respx.mock
async def test_async_google_dns_records_account_level() -> None:
    respx.get(f"{BASE}/email/google/dns").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"dns_records": [{"type": "MX", "value": "smtp.google.com"}]}),
        )
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        records = await c.email.google.dns_records()
    assert len(records) == 1


@pytest.mark.asyncio
@respx.mock
async def test_async_google_setup_admin_posts_body() -> None:
    route = respx.post(f"{BASE}/email/google/example.com/admin").mock(
        return_value=httpx.Response(201, json=_ok({"status": "created"}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        await c.email.google.setup_admin(
            "example.com",
            email_address="a@b.com",
            first_name="A",
            last_name="B",
            alternate_email="alt@b.com",
            name="A B",
            company="C",
            zip="00000",
        )
    assert route.called
    body = route.calls.last.request.read()
    assert b"a@b.com" in body
    assert b"alt@b.com" in body
