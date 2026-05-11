"""Unit tests for the crypto top-up layer (Phase 1.7).

Covers ``TopupInvoice`` and ``AsyncTopupInvoice``: state predicates,
``refresh()`` round-trips, ``wait_until_paid()`` polling-to-completion,
``TopupTimeout`` on stuck invoices, and ``TopupFailed`` on terminal
failure states (cancelled / refunded). Also covers the resource-level
``account.topup()`` and ``account.topup_status()`` integration including
client-side validation.

Mocked via respx — no real API call. ``poll_interval`` is set very low
in tests so the suite runs quickly.
"""

from __future__ import annotations

import asyncio

import httpx
import pytest
import respx

from impreza import (
    AsyncClient,
    AsyncTopupInvoice,
    Client,
    TopupFailed,
    TopupInvoice,
    TopupTimeout,
)
from impreza._topup import build_async_topup_invoice, build_topup_invoice
from impreza.exceptions import ApiError

BASE = "https://api.imprezahost.com/v1"


def _topup_payload(
    invoice_id: int,
    status: str,
    *,
    amount: float = 50.0,
    currency: str = "USD",
    method: str | None = "xmr",
    payment_url: str | None = None,
    expires_at: str | None = None,
    paid_at: str | None = None,
    balance_after: float | None = None,
) -> dict[str, object]:
    data: dict[str, object] = {
        "invoice_id": invoice_id,
        "amount": amount,
        "currency": currency,
        "method": method,
        "status": status,
    }
    if payment_url is not None:
        data["payment_url"] = payment_url
    if expires_at is not None:
        data["expires_at"] = expires_at
    if paid_at is not None:
        data["paid_at"] = paid_at
    if balance_after is not None:
        data["balance_after"] = balance_after
    return {"success": True, "data": data, "meta": {"request_id": "req_test"}}


def _create_payload(invoice_id: int = 12345) -> dict[str, object]:
    return _topup_payload(
        invoice_id,
        "pending",
        payment_url=f"https://portal.imprezahost.com/viewinvoice.php?id={invoice_id}&method=xmr",
        expires_at="2026-05-08T16:30:00Z",
    )


# ── construction ──────────────────────────────────────────────────────


def test_build_topup_invoice_validates_invoice_id_present() -> None:
    http_stub = object()
    payload = {"data": {"status": "pending"}, "meta": {}}
    with pytest.raises(ApiError, match="invoice_id"):
        build_topup_invoice(http_stub, payload)  # type: ignore[arg-type]


def test_build_topup_invoice_validates_status_present() -> None:
    http_stub = object()
    payload = {"data": {"invoice_id": 1}, "meta": {}}
    with pytest.raises(ApiError, match="status"):
        build_topup_invoice(http_stub, payload)  # type: ignore[arg-type]


def test_topup_invoice_exposes_attributes_from_snapshot() -> None:
    with Client(api_key="x", api_secret="y") as c:
        inv = build_topup_invoice(c._http, _create_payload(99))  # noqa: SLF001
    assert inv.invoice_id == 99
    assert inv.status == "pending"
    assert inv.amount == 50.0
    assert inv.currency == "USD"
    assert inv.method == "xmr"
    assert inv.payment_url is not None and "id=99" in inv.payment_url
    assert inv.expires_at == "2026-05-08T16:30:00Z"
    assert inv.paid_at is None
    assert inv.balance_after is None
    assert "99" in repr(inv)
    assert "pending" in repr(inv)


# ── status predicates ─────────────────────────────────────────────────


@pytest.mark.parametrize(
    ("status", "is_done", "is_paid", "is_pending", "is_failed"),
    [
        ("pending", False, False, True, False),
        ("paid", True, True, False, False),
        ("cancelled", True, False, False, True),
        ("canceled", True, False, False, True),
        ("refunded", True, False, False, True),
        ("expired", True, False, False, True),
    ],
)
def test_status_predicates_normalize(
    status: str,
    is_done: bool,
    is_paid: bool,
    is_pending: bool,
    is_failed: bool,
) -> None:
    with Client(api_key="x", api_secret="y") as c:
        inv = build_topup_invoice(c._http, _topup_payload(1, status))  # noqa: SLF001
    assert inv.is_done() is is_done
    assert inv.is_paid() is is_paid
    assert inv.is_pending() is is_pending
    assert inv.is_failed() is is_failed


def test_status_predicates_are_case_insensitive() -> None:
    """Defensive: The API sometimes title-cases statuses upstream."""
    with Client(api_key="x", api_secret="y") as c:
        inv = build_topup_invoice(c._http, _topup_payload(1, "PAID"))  # noqa: SLF001
    assert inv.is_done() and inv.is_paid()


# ── sync wait_until_paid() ────────────────────────────────────────────


@respx.mock
def test_wait_polls_until_paid() -> None:
    respx.get(f"{BASE}/account/topup/100").mock(
        side_effect=[
            httpx.Response(200, json=_topup_payload(100, "pending")),
            httpx.Response(
                200,
                json=_topup_payload(
                    100,
                    "paid",
                    paid_at="2026-05-08T15:42:00Z",
                    balance_after=95.32,
                ),
            ),
        ]
    )
    with Client(api_key="x", api_secret="y") as c:
        inv = build_topup_invoice(c._http, _create_payload(100))  # noqa: SLF001
        result = inv.wait_until_paid(poll_interval=0.01, timeout=2.0)

    assert result is inv
    assert inv.is_paid()
    assert inv.balance_after == 95.32
    assert inv.paid_at == "2026-05-08T15:42:00Z"
    # payment_url survives across status polls
    assert inv.payment_url is not None and "id=100" in inv.payment_url


@respx.mock
def test_wait_returns_immediately_if_already_paid() -> None:
    """No polling happens if the snapshot is already in a terminal state."""
    route = respx.get(f"{BASE}/account/topup/101")
    with Client(api_key="x", api_secret="y") as c:
        inv = build_topup_invoice(  # noqa: SLF001
            c._http,
            _topup_payload(101, "paid", balance_after=20.0),
        )
        inv.wait_until_paid(poll_interval=0.01)
    assert not route.called


@respx.mock
def test_wait_raises_topup_failed_on_cancellation() -> None:
    respx.get(f"{BASE}/account/topup/102").mock(
        return_value=httpx.Response(200, json=_topup_payload(102, "cancelled"))
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(TopupFailed) as exc_info,
    ):
        inv = build_topup_invoice(c._http, _create_payload(102))  # noqa: SLF001
        inv.wait_until_paid(poll_interval=0.01, timeout=2.0)
    assert exc_info.value.invoice.status == "cancelled"  # type: ignore[attr-defined]
    assert "102" in str(exc_info.value)


@respx.mock
def test_wait_raises_topup_failed_on_refund() -> None:
    respx.get(f"{BASE}/account/topup/103").mock(
        return_value=httpx.Response(200, json=_topup_payload(103, "refunded"))
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(TopupFailed) as exc_info,
    ):
        inv = build_topup_invoice(c._http, _create_payload(103))  # noqa: SLF001
        inv.wait_until_paid(poll_interval=0.01, timeout=2.0)
    assert exc_info.value.invoice.status == "refunded"  # type: ignore[attr-defined]


@respx.mock
def test_wait_raises_timeout_when_invoice_doesnt_settle() -> None:
    respx.get(f"{BASE}/account/topup/104").mock(
        return_value=httpx.Response(200, json=_topup_payload(104, "pending"))
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(TopupTimeout) as exc_info,
    ):
        inv = build_topup_invoice(c._http, _create_payload(104))  # noqa: SLF001
        inv.wait_until_paid(poll_interval=0.05, timeout=0.15)
    assert exc_info.value.timeout == 0.15
    assert "104" in str(exc_info.value)


def test_wait_rejects_zero_or_negative_poll_interval() -> None:
    with Client(api_key="x", api_secret="y") as c:
        inv = build_topup_invoice(c._http, _create_payload(105))  # noqa: SLF001
        with pytest.raises(ValueError, match="poll_interval"):
            inv.wait_until_paid(poll_interval=0)


@respx.mock
def test_refresh_returns_self_with_updated_state() -> None:
    respx.get(f"{BASE}/account/topup/106").mock(
        return_value=httpx.Response(
            200,
            json=_topup_payload(
                106, "paid", paid_at="2026-05-08T15:42:00Z", balance_after=80.0
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        inv = build_topup_invoice(c._http, _create_payload(106))  # noqa: SLF001
        same = inv.refresh()

    assert same is inv
    assert inv.is_paid()
    assert inv.balance_after == 80.0
    # payment_url + expires_at carry forward across the refresh
    assert inv.payment_url is not None and "id=106" in inv.payment_url
    assert inv.expires_at == "2026-05-08T16:30:00Z"


# ── async wait_until_paid() ───────────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_wait_polls_until_paid() -> None:
    respx.get(f"{BASE}/account/topup/200").mock(
        side_effect=[
            httpx.Response(200, json=_topup_payload(200, "pending")),
            httpx.Response(
                200,
                json=_topup_payload(200, "paid", balance_after=100.0),
            ),
        ]
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        inv = build_async_topup_invoice(c._http, _create_payload(200))  # noqa: SLF001
        result = await inv.wait_until_paid(poll_interval=0.01, timeout=2.0)

    assert result is inv
    assert isinstance(inv, AsyncTopupInvoice)
    assert inv.is_paid()
    assert inv.balance_after == 100.0


@pytest.mark.asyncio
@respx.mock
async def test_async_wait_raises_topup_failed() -> None:
    respx.get(f"{BASE}/account/topup/201").mock(
        return_value=httpx.Response(200, json=_topup_payload(201, "cancelled"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        inv = build_async_topup_invoice(c._http, _create_payload(201))  # noqa: SLF001
        with pytest.raises(TopupFailed) as exc_info:
            await inv.wait_until_paid(poll_interval=0.01, timeout=2.0)
    assert "201" in str(exc_info.value)


@pytest.mark.asyncio
@respx.mock
async def test_async_wait_raises_timeout() -> None:
    respx.get(f"{BASE}/account/topup/202").mock(
        return_value=httpx.Response(200, json=_topup_payload(202, "pending"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        inv = build_async_topup_invoice(c._http, _create_payload(202))  # noqa: SLF001
        with pytest.raises(TopupTimeout):
            await inv.wait_until_paid(poll_interval=0.05, timeout=0.15)


@pytest.mark.asyncio
@respx.mock
async def test_async_refresh_updates_state() -> None:
    respx.get(f"{BASE}/account/topup/203").mock(
        return_value=httpx.Response(
            200, json=_topup_payload(203, "paid", balance_after=42.0)
        )
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        inv = build_async_topup_invoice(c._http, _create_payload(203))  # noqa: SLF001
        await inv.refresh()
    assert inv.is_paid()
    assert inv.balance_after == 42.0


# ── resource integration: account.topup() / topup_status() ───────────


@respx.mock
def test_account_topup_returns_typed_future() -> None:
    route = respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_create_payload(500))
    )
    with Client(api_key="x", api_secret="y") as c:
        inv = c.account.topup(amount=50, method="xmr")
    assert isinstance(inv, TopupInvoice)
    assert inv.invoice_id == 500
    assert inv.status == "pending"
    assert inv.payment_url is not None
    # Body contained both fields
    body = route.calls.last.request.read()
    assert b"50" in body
    assert b"xmr" in body


@respx.mock
def test_account_topup_omits_method_when_none() -> None:
    route = respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_create_payload(501))
    )
    with Client(api_key="x", api_secret="y") as c:
        c.account.topup(amount=25)
    body = route.calls.last.request.read()
    # method shouldn't be in the JSON when not specified
    assert b"method" not in body
    assert b"25" in body


def test_account_topup_rejects_amount_out_of_range() -> None:
    with Client(api_key="x", api_secret="y") as c:
        with pytest.raises(ValueError, match="amount"):
            c.account.topup(amount=0.5)
        with pytest.raises(ValueError, match="amount"):
            c.account.topup(amount=10001)


def test_account_topup_rejects_unknown_method() -> None:
    with Client(api_key="x", api_secret="y") as c, pytest.raises(ValueError, match="method"):
        c.account.topup(amount=50, method="eth")


@respx.mock
def test_account_topup_status_returns_typed_future() -> None:
    respx.get(f"{BASE}/account/topup/600").mock(
        return_value=httpx.Response(
            200,
            json=_topup_payload(
                600, "paid", paid_at="2026-05-08T15:42:00Z", balance_after=70.0
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        inv = c.account.topup_status(600)
    assert isinstance(inv, TopupInvoice)
    assert inv.invoice_id == 600
    assert inv.is_paid()
    assert inv.balance_after == 70.0


@pytest.mark.asyncio
@respx.mock
async def test_async_account_topup_returns_async_future() -> None:
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_create_payload(700))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        inv = await c.account.topup(amount=50, method="btc")
    assert isinstance(inv, AsyncTopupInvoice)
    assert inv.invoice_id == 700


@pytest.mark.asyncio
@respx.mock
async def test_async_account_topup_status_returns_async_future() -> None:
    respx.get(f"{BASE}/account/topup/701").mock(
        return_value=httpx.Response(200, json=_topup_payload(701, "pending"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        inv = await c.account.topup_status(701)
    assert isinstance(inv, AsyncTopupInvoice)
    assert inv.invoice_id == 701
    assert inv.is_pending()


@respx.mock
def test_account_topup_then_wait_full_lifecycle() -> None:
    """End-to-end UX: c.account.topup(...).wait_until_paid() — one call away."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_create_payload(800))
    )
    respx.get(f"{BASE}/account/topup/800").mock(
        side_effect=[
            httpx.Response(200, json=_topup_payload(800, "pending")),
            httpx.Response(
                200, json=_topup_payload(800, "paid", balance_after=150.0)
            ),
        ]
    )
    with Client(api_key="x", api_secret="y") as c:
        inv = c.account.topup(amount=100, method="usdt_trc20")
        inv.wait_until_paid(poll_interval=0.01, timeout=2.0)
    assert inv.is_paid()
    assert inv.balance_after == 150.0
    # The original payment_url is still accessible after the wait loop
    assert inv.payment_url is not None and "id=800" in inv.payment_url


# Used by pytest-asyncio implicitly; the bare reference silences unused-import lints.
_ = asyncio
