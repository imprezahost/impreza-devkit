"""Unit tests for ``AsyncClient`` and ``AsyncAccountResource``.

Mocked via ``respx`` — no real API call. Mirrors
``test_account_unit.py`` but exercises the async surface.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import (
    AccountInfo,
    AsyncClient,
    AuthError,
    ImprezaError,
    IpNotWhitelisted,
    PermissionDenied,
    RateLimitExceeded,
    ResourceNotFound,
)

BASE = "https://api.imprezahost.com/v1"


def _ok_account_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": 1234,
            "first_name": "John",
            "last_name": "Doe",
            "company": "ACME Corp",
            "email": "john@example.com",
            "balance": 150.0,
            "currency": "USD",
            "registered_at": "2024-01-15",
        },
        "meta": {"request_id": "req_test", "timestamp": "2026-05-08T17:03:00Z"},
    }


def _error_payload(code: str, message: str) -> dict[str, object]:
    return {
        "success": False,
        "error": {"code": code, "message": message},
        "meta": {"request_id": "req_test"},
    }


@pytest.mark.asyncio
@respx.mock
async def test_async_account_get_returns_model() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok_account_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        account = await c.account.get()

    assert isinstance(account, AccountInfo)
    assert account.id == 1234
    assert account.email == "john@example.com"
    assert account.balance == 150.0


@pytest.mark.asyncio
@respx.mock
async def test_async_account_sends_auth_headers() -> None:
    route = respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok_account_payload())
    )

    async with AsyncClient(api_key="imp_xxx", api_secret="sek_yyy") as c:
        await c.account.get()

    assert route.called
    sent = route.calls.last.request
    assert sent.headers["X-API-Key"] == "imp_xxx"
    assert sent.headers["X-API-Secret"] == "sek_yyy"
    assert sent.headers["User-Agent"].startswith("impreza-sdk-python/")


@pytest.mark.asyncio
@respx.mock
async def test_async_401_raises_auth_error() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(401, json=_error_payload("UNAUTHORIZED", "Invalid API key."))
    )

    async with AsyncClient(api_key="bad", api_secret="bad", max_retries=0) as c:
        with pytest.raises(AuthError) as exc_info:
            await c.account.get()

    err = exc_info.value
    assert err.status_code == 401
    assert err.code == "UNAUTHORIZED"
    assert err.request_id == "req_test"
    assert isinstance(err, ImprezaError)


@pytest.mark.asyncio
@respx.mock
async def test_async_403_with_ip_code_raises_ip_not_whitelisted() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            403,
            json=_error_payload("ip_not_whitelisted", "Your IP is not in the whitelist."),
        )
    )

    async with AsyncClient(api_key="x", api_secret="y", max_retries=0) as c:
        with pytest.raises(IpNotWhitelisted) as exc_info:
            await c.account.get()

    assert isinstance(exc_info.value, PermissionDenied)


@pytest.mark.asyncio
@respx.mock
async def test_async_404_raises_resource_not_found() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(404, json=_error_payload("NOT_FOUND", "Not found."))
    )

    async with AsyncClient(api_key="x", api_secret="y", max_retries=0) as c:
        with pytest.raises(ResourceNotFound):
            await c.account.get()


@pytest.mark.asyncio
@respx.mock
async def test_async_429_retries_with_retry_after() -> None:
    route = respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            429,
            headers={"Retry-After": "0"},
            json=_error_payload("RATE_LIMITED", "Slow down."),
        )
    )

    async with AsyncClient(api_key="x", api_secret="y", max_retries=1) as c:
        with pytest.raises(RateLimitExceeded) as exc_info:
            await c.account.get()

    assert route.call_count == 2
    assert exc_info.value.retry_after == 0
