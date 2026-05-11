"""Unit tests for ``c.invoices`` (sync + async).

Mocked via ``respx`` — no real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import AsyncClient, Client, Invoice, InvoiceDetail

BASE = "https://api.imprezahost.com/v1"


def _invoices_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "invoices": [
                {
                    "id": 2345,
                    "invoice_num": "2345",
                    "date": "2026-03-01",
                    "due_date": "2026-03-15",
                    "date_paid": None,
                    "subtotal": 17.0,
                    "credit": 0.0,
                    "tax": 0.0,
                    "total": 17.0,
                    "status": "Unpaid",
                    "payment_method": "banktransfer",
                },
            ],
            "total": 1,
        },
        "meta": {"request_id": "req_test"},
    }


def _invoice_detail_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": 2345,
            "invoice_num": "2345",
            "date": "2026-03-01",
            "due_date": "2026-03-15",
            "total": 17.0,
            "status": "Unpaid",
            "items": [
                {
                    "id": 1,
                    "type": "Hosting",
                    "description": "VPS Plan 2 - Monthly",
                    "amount": 17.0,
                    "taxed": False,
                },
            ],
            "transactions": [],
        },
        "meta": {"request_id": "req_test"},
    }


@respx.mock
def test_invoices_list() -> None:
    respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(200, json=_invoices_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        invoices = c.invoices.list()

    assert len(invoices) == 1
    assert isinstance(invoices[0], Invoice)
    assert invoices[0].id == 2345
    assert invoices[0].status == "Unpaid"
    assert invoices[0].total == 17.0


@respx.mock
def test_invoices_list_sends_status_filter() -> None:
    route = respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(200, json=_invoices_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.invoices.list(status="Unpaid")

    sent = route.calls.last.request
    assert sent.url.params.get("status") == "Unpaid"


@respx.mock
def test_invoices_get_returns_detail_with_items() -> None:
    respx.get(f"{BASE}/invoices/2345").mock(
        return_value=httpx.Response(200, json=_invoice_detail_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        invoice = c.invoices.get(2345)

    assert isinstance(invoice, InvoiceDetail)
    assert invoice.id == 2345
    assert len(invoice.items) == 1
    assert invoice.items[0].description.startswith("VPS Plan 2")
    assert invoice.transactions == []


@respx.mock
def test_invoices_list_empty() -> None:
    respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {"invoices": [], "total": 0},
                "meta": {"request_id": "req_test"},
            },
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        assert c.invoices.list() == []


@pytest.mark.asyncio
@respx.mock
async def test_async_invoices_list() -> None:
    respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(200, json=_invoices_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        invoices = await c.invoices.list()

    assert invoices[0].id == 2345


@pytest.mark.asyncio
@respx.mock
async def test_async_invoices_get() -> None:
    respx.get(f"{BASE}/invoices/2345").mock(
        return_value=httpx.Response(200, json=_invoice_detail_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        invoice = await c.invoices.get(2345)

    assert invoice.items[0].amount == 17.0


@pytest.mark.asyncio
@respx.mock
async def test_async_invoices_list_sends_status_filter() -> None:
    """Async parity for the status-filter param plumbing — the
    sync side asserts the query param flows through, the async
    counterpart must too."""
    route = respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(200, json=_invoices_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        await c.invoices.list(status="Unpaid")

    sent = route.calls.last.request
    assert sent.url.params.get("status") == "Unpaid"
