"""Invoice response models."""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict


class Invoice(BaseModel):
    """Invoice summary as returned by ``GET /invoices``.

    For full line items + transaction history, fetch via ``invoices.get(id)``
    which returns :class:`InvoiceDetail`.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    invoice_num: str
    date: str
    due_date: str | None = None
    date_paid: str | None = None
    subtotal: float = 0.0
    credit: float = 0.0
    tax: float = 0.0
    total: float
    status: str
    payment_method: str | None = None


class InvoiceItem(BaseModel):
    """A single line item on an invoice."""

    model_config = ConfigDict(extra="ignore")

    id: int
    type: str | None = None
    description: str
    amount: float
    taxed: bool = False


class InvoiceTransaction(BaseModel):
    """A payment transaction recorded against an invoice."""

    model_config = ConfigDict(extra="ignore")

    id: int | None = None
    date: str | None = None
    gateway: str | None = None
    amount: float | None = None
    transaction_id: str | None = None


class InvoiceDetail(Invoice):
    """Full invoice with line items and transaction history."""

    model_config = ConfigDict(extra="ignore")

    items: list[InvoiceItem] = []
    transactions: list[InvoiceTransaction] = []
