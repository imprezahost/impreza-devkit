"""Invoices resource — accessed via ``Client.invoices``.

Phase 1.3 ships read access (list / get). Pay-from-balance and other
write operations land in Phase 1.4 / 1.7.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from ..models.invoice import Invoice, InvoiceDetail

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


def _list_params(status: str | None) -> dict[str, object] | None:
    return {"status": status} if status else None


class InvoicesResource:
    """Sync read access to the authenticated client's invoices."""

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


class AsyncInvoicesResource:
    """Async read access to the authenticated client's invoices."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self, *, status: str | None = None) -> list[Invoice]:
        payload = await self._http.get("/invoices", params=_list_params(status))
        return _extract_invoices(payload)

    async def get(self, invoice_id: int) -> InvoiceDetail:
        payload = await self._http.get(f"/invoices/{invoice_id}")
        return _extract_invoice_detail(payload)
