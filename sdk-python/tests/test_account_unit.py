"""Unit tests for the ``account`` resource and HTTP envelope handling.

These tests use ``respx`` to mock httpx — no real API is called. They
cover the success path and the main error mappings.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import (
    AccountInfo,
    AuthError,
    Client,
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


@respx.mock
def test_account_get_returns_model() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok_account_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        account = c.account.get()

    assert isinstance(account, AccountInfo)
    assert account.id == 1234
    assert account.email == "john@example.com"
    assert account.balance == 150.0
    assert account.currency == "USD"
    assert account.company == "ACME Corp"


@respx.mock
def test_account_sends_auth_headers() -> None:
    route = respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(200, json=_ok_account_payload())
    )

    with Client(api_key="imp_xxx", api_secret="sek_yyy") as c:
        c.account.get()

    assert route.called
    sent = route.calls.last.request
    assert sent.headers["X-API-Key"] == "imp_xxx"
    assert sent.headers["X-API-Secret"] == "sek_yyy"
    assert sent.headers["Accept"] == "application/json"
    assert sent.headers["User-Agent"].startswith("impreza-sdk-python/")


@respx.mock
def test_401_raises_auth_error() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(401, json=_error_payload("UNAUTHORIZED", "Invalid API key."))
    )

    with (
        Client(api_key="bad", api_secret="bad", max_retries=0) as c,
        pytest.raises(AuthError) as exc_info,
    ):
        c.account.get()

    err = exc_info.value
    assert err.status_code == 401
    assert err.code == "UNAUTHORIZED"
    assert err.request_id == "req_test"
    assert "Invalid API key" in str(err)
    assert isinstance(err, ImprezaError)


@respx.mock
def test_403_with_ip_code_raises_ip_not_whitelisted() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            403,
            json=_error_payload("ip_not_whitelisted", "Your IP is not in the whitelist."),
        )
    )

    with (
        Client(api_key="x", api_secret="y", max_retries=0) as c,
        pytest.raises(IpNotWhitelisted) as exc_info,
    ):
        c.account.get()

    # IpNotWhitelisted should be a subclass of PermissionDenied so existing
    # 403 handlers keep matching.
    assert isinstance(exc_info.value, PermissionDenied)


@respx.mock
def test_403_without_ip_code_raises_permission_denied() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(403, json=_error_payload("FORBIDDEN", "Forbidden."))
    )

    with (
        Client(api_key="x", api_secret="y", max_retries=0) as c,
        pytest.raises(PermissionDenied) as exc_info,
    ):
        c.account.get()

    assert not isinstance(exc_info.value, IpNotWhitelisted)


@respx.mock
def test_404_raises_resource_not_found() -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(404, json=_error_payload("NOT_FOUND", "Not found."))
    )

    with (
        Client(api_key="x", api_secret="y", max_retries=0) as c,
        pytest.raises(ResourceNotFound),
    ):
        c.account.get()


@respx.mock
def test_429_retries_with_retry_after_then_raises() -> None:
    route = respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            429,
            headers={"Retry-After": "0"},
            json=_error_payload("RATE_LIMITED", "Slow down."),
        )
    )

    with (
        Client(api_key="x", api_secret="y", max_retries=1) as c,
        pytest.raises(RateLimitExceeded) as exc_info,
    ):
        c.account.get()

    # max_retries=1 means: initial attempt + 1 retry = 2 total calls.
    assert route.call_count == 2
    assert exc_info.value.retry_after == 0
    assert exc_info.value.status_code == 429


@respx.mock
def test_5xx_retries_then_raises_server_error() -> None:
    from impreza import ServerError

    route = respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(500, json=_error_payload("SERVER_ERROR", "Boom."))
    )

    # Use max_retries=1 with a tiny client that does not actually wait.
    # The HttpClient backoff sleeps real time; for unit-test speed, set
    # max_retries low.
    with Client(api_key="x", api_secret="y", max_retries=1) as c, pytest.raises(ServerError):
        c.account.get()

    assert route.call_count == 2


# ── api_key_self ──────────────────────────────────────────────────────


@respx.mock
def test_api_key_self_round_trips() -> None:
    """``c.account.api_key_self()`` decodes the GET /account/api-keys/self
    payload into a typed :class:`KeyIdentity` with the IP whitelist as
    typed entries."""
    from impreza import IpWhitelistEntry, KeyIdentity

    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200,
            json={
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
                    "ip_whitelist": [
                        {
                            "id": 6,
                            "ip_address": "64.31.49.102",
                            "label": "office",
                            "created_at": "2026-04-01 13:04:40",
                        },
                        {
                            "id": 16,
                            "ip_address": "203.0.113.42",
                            "label": "home",
                            "created_at": "2026-05-08 18:49:49",
                        },
                    ],
                    "request_ip": "203.0.113.42",
                },
            },
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        ident = c.account.api_key_self()

    assert isinstance(ident, KeyIdentity)
    assert ident.id == 21
    assert ident.client_id == 1
    assert ident.prefix == "imp_a1b2c3d4"
    assert ident.label == "ci-bot"
    assert ident.status == "active"
    assert ident.rate_limit_per_minute == 60
    assert ident.request_ip == "203.0.113.42"
    assert len(ident.ip_whitelist) == 2
    assert isinstance(ident.ip_whitelist[0], IpWhitelistEntry)
    assert ident.ip_whitelist[0].ip_address == "64.31.49.102"
    assert ident.ip_whitelist[1].label == "home"
