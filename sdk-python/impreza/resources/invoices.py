"""Invoices resource — accessed via ``Client.invoices``.

Read access (``list`` / ``get``) plus pay-from-balance (``pay``).

``pay`` spends real account credit and is not reversible from here, so
it takes no ``confirm`` flag by design — the decision belongs to the
caller, and every CLI/UI on top of this prompts before calling it.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from ..models.invoice import Invoice, InvoiceDetail, InvoicePayment

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


def _extract_invoices(payload: dict[str, object]) -> list[Invoice]:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    items_raw = data.get("invoices")
    items = items_raw if isinstance(items_raw, list) else []
    return [Invoice.model_validate(item) for item in items]


def _extract_invoice_detail(payload: dict[str, object]) -> InvoiceDetail:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    return InvoiceDetail.model_validate(data)


def _extract_payment(payload: dict[str, object], invoice_id: int) -> InvoicePayment:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    # The endpoint echoes invoice_id, but fall back to the one we asked
    # about so the result is never ambiguous about which invoice moved.
    data.setdefault("invoice_id", invoice_id)
    return InvoicePayment.model_validate(data)


def _list_params(status: str | None) -> dict[str, object] | None:
    return {"status": status} if status else None


class InvoicesResource:
    """Sync access to the authenticated client's invoices."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self, *, status: str | None = None) -> list[Invoice]:
        """List invoices, optionally filtered by status.

        ``status`` accepts these values: ``Paid``, ``Unpaid``, ``Cancelled``,
        ``Refunded``.
        """
        payload = self._http.get("/invoices", params=_list_params(status))
        return _extract_invoices(payload)

    def get(self, invoice_id: int) -> InvoiceDetail:
        """Return one invoice with its line items and transaction history."""
        payload = self._http.get(f"/invoices/{invoice_id}")
        return _extract_invoice_detail(payload)

    def pay(self, invoice_id: int) -> InvoicePayment:
        """Pay an unpaid invoice from the account credit balance.

        This spends real money and cannot be undone through the API.

        Raises:
            Conflict: 409 — the invoice is already paid
                (``code == "ALREADY_PAID"``) or the balance does not cover
                it (``code == "INSUFFICIENT_BALANCE"``). Top up first with
                ``client.account.topup(...)``.
            ResourceNotFound: 404 — no such invoice on this account.
        """
        payload = self._http.post(f"/invoices/{invoice_id}/pay")
        return _extract_payment(payload, invoice_id)


class AsyncInvoicesResource:
    """Async access to the authenticated client's invoices."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self, *, status: str | None = None) -> list[Invoice]:
        payload = await self._http.get("/invoices", params=_list_params(status))
        return _extract_invoices(payload)

    async def get(self, invoice_id: int) -> InvoiceDetail:
        payload = await self._http.get(f"/invoices/{invoice_id}")
        return _extract_invoice_detail(payload)

    async def pay(self, invoice_id: int) -> InvoicePayment:
        """Pay an unpaid invoice from the account credit balance.

        Spends real money; see :meth:`InvoicesResource.pay`.
        """
        payload = await self._http.post(f"/invoices/{invoice_id}/pay")
        return _extract_payment(payload, invoice_id)
