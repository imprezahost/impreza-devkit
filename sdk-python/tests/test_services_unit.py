"""Unit tests for ``c.account.services`` (sync + async).

Mocked via ``respx`` — no real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import AsyncClient, Client, ResourceNotFound, Service

BASE = "https://api.imprezahost.com/v1"


def _services_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "services": [
                {
                    "id": 567,
                    "domain": "example.com",
                    "status": "Active",
                    "product": "VPS Plan 2",
                    "product_group": "VPS Hosting",
                    "billing_cycle": "monthly",
                    "amount": 15.0,
                    "dedicated_ip": "185.100.86.42",
                    "registered_at": "2024-06-01",
                    "next_due": "2026-04-01",
                },
                {
                    "id": 568,
                    "domain": "another.net",
                    "status": "Active",
                    "product": "Hosting Pro",
                    "product_group": "Shared Hosting",
                },
            ],
            "total": 2,
        },
        "meta": {"request_id": "req_test"},
    }


def _service_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": 567,
            "domain": "example.com",
            "status": "Active",
            "product": "VPS Plan 2",
        },
        "meta": {"request_id": "req_test"},
    }


@respx.mock
def test_services_list_parses_array() -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(200, json=_services_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        services = c.account.services.list()

    assert len(services) == 2
    assert all(isinstance(s, Service) for s in services)
    assert services[0].id == 567
    assert services[0].product == "VPS Plan 2"
    assert services[0].dedicated_ip == "185.100.86.42"
    assert services[1].domain == "another.net"


@respx.mock
def test_services_list_sends_status_filter() -> None:
    route = respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(200, json=_services_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.account.services.list(status="Active")

    sent = route.calls.last.request
    assert sent.url.params.get("status") == "Active"


@respx.mock
def test_services_get_returns_single_service() -> None:
    respx.get(f"{BASE}/account/services/567").mock(
        return_value=httpx.Response(200, json=_service_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        svc = c.account.services.get(567)

    assert svc.id == 567
    assert svc.product == "VPS Plan 2"


@respx.mock
def test_services_get_404_raises_resource_not_found() -> None:
    respx.get(f"{BASE}/account/services/9999").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "error": {"code": "NOT_FOUND", "message": "Service not found."},
                "meta": {"request_id": "req_test"},
            },
        )
    )

    with (
        Client(api_key="x", api_secret="y", max_retries=0) as c,
        pytest.raises(ResourceNotFound),
    ):
        c.account.services.get(9999)


@respx.mock
def test_services_list_empty_data_returns_empty() -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {"services": [], "total": 0},
                "meta": {"request_id": "req_test"},
            },
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        assert c.account.services.list() == []


@pytest.mark.asyncio
@respx.mock
async def test_async_services_list() -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(200, json=_services_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        services = await c.account.services.list()

    assert len(services) == 2
    assert services[0].id == 567


@pytest.mark.asyncio
@respx.mock
async def test_async_services_get() -> None:
    respx.get(f"{BASE}/account/services/567").mock(
        return_value=httpx.Response(200, json=_service_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        svc = await c.account.services.get(567)

    assert svc.id == 567
    assert svc.product == "VPS Plan 2"


# ── cancel (Phase 3.7) ─────────────────────────────────────────────────


@respx.mock
def test_services_cancel_immediate() -> None:
    """``Immediate`` cancellation sends just `type` in the body — no
    reason — and returns None on success."""
    route = respx.post(f"{BASE}/services/567/cancel").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "service_id": 567,
                    "cancel_type": "Immediate",
                    "message": "Cancellation request submitted successfully.",
                },
                "meta": {"request_id": "req_test"},
            },
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.account.services.cancel(567, type="Immediate")

    assert result is None
    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {"type": "Immediate"}


@respx.mock
def test_services_cancel_end_of_billing_with_reason() -> None:
    """End of Billing Period + reason — both fields in the body."""
    route = respx.post(f"{BASE}/services/567/cancel").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {},
                "meta": {"request_id": "req_test"},
            },
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        c.account.services.cancel(
            567, type="End of Billing Period", reason="moving providers"
        )

    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {
        "type": "End of Billing Period",
        "reason": "moving providers",
    }


def test_services_cancel_invalid_type_raises_value_error() -> None:
    """Client-side validation (in the SDK helper) rejects unknown
    type values before any HTTP call."""
    with Client(api_key="x", api_secret="y") as c, pytest.raises(ValueError):
        c.account.services.cancel(567, type="Whenever")


@pytest.mark.asyncio
@respx.mock
async def test_async_services_cancel() -> None:
    route = respx.post(f"{BASE}/services/567/cancel").mock(
        return_value=httpx.Response(
            200, json={"success": True, "data": {}, "meta": {"request_id": "x"}}
        )
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        await c.account.services.cancel(
            567, type="Immediate", reason="testing"
        )

    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {"type": "Immediate", "reason": "testing"}
