"""Order-related response models (Phase 1.4d)."""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field


class Order(BaseModel):
    """An order summary returned by ``GET /orders``.

    ``order_number`` is the system-generated reference shown in the admin
    UI. The API sometimes returns it as an int (10 digits) and
    sometimes as a string ("ORD-0001") depending on configuration — both
    are accepted.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    order_number: int | str | None = None
    date: str | None = None
    amount: float
    invoice_id: int | None = None
    status: str
    payment_method: str | None = None


class OrderItem(BaseModel):
    """A single line item inside an order — one provisioned service.

    Returned as part of :class:`OrderDetail`.
    """

    model_config = ConfigDict(extra="ignore")

    service_id: int
    domain: str | None = None
    product: str | None = None
    status: str | None = None
    billing_cycle: str | None = None
    amount: float | None = None


class OrderDetail(Order):
    """An order plus its line items, returned by ``GET /orders/{id}``."""

    model_config = ConfigDict(extra="ignore")

    items: list[OrderItem] = Field(default_factory=list)


class OrderResult(BaseModel):
    """The response of ``POST /orders`` (create) or ``POST /orders/{id}/upgrade``.

    ``order_id`` and ``invoice_id`` identify the records created.
    ``status`` reports the order state immediately after creation
    (typically ``"Active"`` once the order is accepted, but may be
    ``"Pending"`` during async provisioning).
    """

    model_config = ConfigDict(extra="ignore")

    order_id: int
    invoice_id: int
    amount: float
    currency: str
    status: str
    product: str | None = None
    message: str | None = None
