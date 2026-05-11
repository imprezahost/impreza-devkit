"""Domain-related response models.

The shape of ``GET /domains/{domain}`` is loosely specified in the OpenAPI
("full domain details"); we model it defensively — most fields optional —
so adding/removing fields server-side does not break clients.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict


class Domain(BaseModel):
    """Full domain detail returned by ``GET /domains/{domain}``.

    All fields except ``domain`` are optional because the upstream
    response varies by registrar/TLD — some carry GDPR auth state,
    others do not, etc. ``model_config`` ignores unknown fields so new
    server-side additions are forward-compatible.
    """

    model_config = ConfigDict(extra="ignore")

    domain: str
    status: str | None = None
    registration_date: str | None = None
    next_due_date: str | None = None
    expires_at: str | None = None
    nameservers: list[str] = []
    lock_status: bool | None = None
    id_protection: bool | None = None
    auto_renew: bool | None = None
    epp_code: str | None = None
    privacy: bool | None = None


class DomainRegistration(BaseModel):
    """Result of ``POST /domains/register``.

    Successful responses include ``order_id`` and ``invoice_id`` referring
    to the records the registration created.
    """

    model_config = ConfigDict(extra="ignore")

    order_id: int
    invoice_id: int
    domain: str
    years: int
    amount: float
    currency: str
    status: str | None = None
    message: str | None = None


class DomainTransfer(BaseModel):
    """Result of ``POST /domains/transfer``."""

    model_config = ConfigDict(extra="ignore")

    order_id: int
    invoice_id: int
    domain: str
    years: int
    amount: float
    currency: str
    status: str | None = None
    message: str | None = None
