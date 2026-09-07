"""Unit tests for ``c.invoices`` (sync + async).

Mocked via ``respx`` — no real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import (
    ApiError,
    AsyncClient,
    Client,
    Conflict,
    Invoice,
    InvoiceDetail,
    ResourceNotFound,
)

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


# ── pay from balance ───────────────────────────────────────────────────


def _payment_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "invoice_id": 2345,
            "amount": 17.0,
            "currency": "USD",
            "message": "Invoice paid from account balance.",
        },
        "meta": {"request_id": "req_test"},
    }


def _conflict(code: str, message: str) -> dict[str, object]:
    return {
        "success": False,
        "error": {"code": code, "message": message},
        "meta": {"request_id": "req_test"},
    }


@respx.mock
def test_invoices_pay_posts_and_parses() -> None:
    route = respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=_payment_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.invoices.pay(2345)

    assert route.called
    assert route.calls.last.request.method == "POST"
    assert result.invoice_id == 2345
    assert result.amount == 17.0
    assert result.currency == "USD"


@respx.mock
def test_invoices_pay_backfills_invoice_id() -> None:
    """A server that omits invoice_id must not yield an ambiguous
    result — the id we asked about is filled in."""
    payload = _payment_payload()
    del payload["data"]["invoice_id"]  # type: ignore[index]
    respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=payload)
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.invoices.pay(2345)

    assert result.invoice_id == 2345


@pytest.mark.parametrize(
    ("code", "message"),
    [
        ("ALREADY_PAID", "This invoice is already paid."),
        ("INSUFFICIENT_BALANCE", "Insufficient balance. Required: USD 17.00"),
    ],
)
@respx.mock
def test_invoices_pay_409_raises_conflict(code: str, message: str) -> None:
    """Both 409 cases surface as Conflict, distinguishable by ``code`` —
    a caller must be able to tell "nothing to do" from "top up first"."""
    respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(409, json=_conflict(code, message))
    )

    with Client(api_key="x", api_secret="y") as c, pytest.raises(Conflict) as exc:
        c.invoices.pay(2345)

    assert exc.value.code == code
    assert exc.value.status_code == 409
    assert isinstance(exc.value, ApiError)


@respx.mock
def test_invoices_pay_404_raises_not_found() -> None:
    respx.post(f"{BASE}/invoices/999/pay").mock(
        return_value=httpx.Response(404, json=_conflict("NOT_FOUND", "No such invoice."))
    )

    with Client(api_key="x", api_secret="y") as c, pytest.raises(ResourceNotFound):
        c.invoices.pay(999)


@pytest.mark.asyncio
@respx.mock
async def test_async_invoices_pay() -> None:
    """Async parity, including the 409 mapping — the two transports have
    separate exception tables, so this has to be asserted twice."""
    route = respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=_payment_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        result = await c.invoices.pay(2345)

    assert route.called
    assert result.amount == 17.0


@pytest.mark.asyncio
@respx.mock
async def test_async_invoices_pay_409_raises_conflict() -> None:
    respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(409, json=_conflict("ALREADY_PAID", "Already paid."))
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        with pytest.raises(Conflict):
            await c.invoices.pay(2345)
