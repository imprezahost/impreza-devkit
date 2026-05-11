"""Unit tests for the hosting resource (Phase 1.4c).

Three endpoints, sync + async, mocked via respx — no real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import AsyncClient, Client

BASE = "https://api.imprezahost.com/v1"


def _ok(data: dict[str, object] | list[object]) -> dict[str, object]:
    return {
        "success": True,
        "data": data,  # type: ignore[dict-item]
        "meta": {"request_id": "req_test"},
    }


# ── sync ───────────────────────────────────────────────────────────────


@respx.mock
def test_hosting_get_returns_account_summary_dict() -> None:
    respx.get(f"{BASE}/hosting/15957").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "ip": "208.115.225.138",
                    "plan": "USA Linux Hosting III",
                    "disk_used": 1234,
                    "disk_limit": 50000,
                    "bw_used": 100,
                    "bw_limit": 100000,
                    "status": "active",
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        info = c.hosting.get(15957)
    assert info["ip"] == "208.115.225.138"
    assert info["plan"] == "USA Linux Hosting III"
    assert info["status"] == "active"


@respx.mock
def test_hosting_get_handles_unexpected_shape_gracefully() -> None:
    """If the API returns a non-dict data field, .get() falls back to {} ."""
    respx.get(f"{BASE}/hosting/9999").mock(
        return_value=httpx.Response(200, json={"success": True, "data": None, "meta": {}})
    )
    with Client(api_key="x", api_secret="y") as c:
        info = c.hosting.get(9999)
    assert info == {}


@respx.mock
def test_hosting_nameservers_returns_list_of_strings() -> None:
    respx.get(f"{BASE}/hosting/15957/nameservers").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"nameservers": ["ns1.imprezahost.com", "ns2.imprezahost.com"]}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        ns = c.hosting.nameservers(15957)
    assert ns == ["ns1.imprezahost.com", "ns2.imprezahost.com"]


@respx.mock
def test_hosting_nameservers_returns_empty_list_when_missing() -> None:
    respx.get(f"{BASE}/hosting/15957/nameservers").mock(
        return_value=httpx.Response(200, json=_ok({}))
    )
    with Client(api_key="x", api_secret="y") as c:
        assert c.hosting.nameservers(15957) == []


@respx.mock
def test_hosting_trigger_autossl_posts_and_returns_status() -> None:
    route = respx.post(f"{BASE}/hosting/15957/autossl").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"message": "AutoSSL check initiated.", "details": {"queued": True}}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        result = c.hosting.trigger_autossl(15957)
    assert route.called
    assert result["message"] == "AutoSSL check initiated."
    assert isinstance(result["details"], dict)


# ── async ──────────────────────────────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_hosting_get() -> None:
    respx.get(f"{BASE}/hosting/15957").mock(
        return_value=httpx.Response(200, json=_ok({"ip": "10.0.0.1", "plan": "x"}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        info = await c.hosting.get(15957)
    assert info["ip"] == "10.0.0.1"


@pytest.mark.asyncio
@respx.mock
async def test_async_hosting_nameservers() -> None:
    respx.get(f"{BASE}/hosting/15957/nameservers").mock(
        return_value=httpx.Response(200, json=_ok({"nameservers": ["ns1.x", "ns2.x"]}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        ns = await c.hosting.nameservers(15957)
    assert ns == ["ns1.x", "ns2.x"]


@pytest.mark.asyncio
@respx.mock
async def test_async_hosting_trigger_autossl() -> None:
    route = respx.post(f"{BASE}/hosting/15957/autossl").mock(
        return_value=httpx.Response(200, json=_ok({"message": "ok"}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        result = await c.hosting.trigger_autossl(15957)
    assert route.called and result["message"] == "ok"
