"""Account-related response models."""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict


class AccountInfo(BaseModel):
    """Authenticated client's profile and balance.

    Returned by ``GET /account``. Shape mirrors the ``AccountInfo`` schema
    in ``openapi/openapi.yaml``.

    Unknown fields from the API are silently ignored so adding new fields
    server-side does not break existing clients.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    first_name: str
    last_name: str
    company: str | None = None
    email: str
    balance: float
    currency: str
    registered_at: str


class IpWhitelistEntry(BaseModel):
    """A single IP entry on the API key's whitelist.

    Returned as part of :class:`KeyIdentity.ip_whitelist`.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    ip_address: str
    label: str | None = None
    created_at: str


class KeyIdentity(BaseModel):
    """Identity, scopes, and IP whitelist of the API key that
    authenticated the current request.

    Returned by ``GET /account/api-keys/self``. The full ``api_key``
    value is **never** returned — only the public ``prefix`` (first
    12 chars). ``request_ip`` is the IP the server observed for the
    call, useful when debugging which IP needs to be on the whitelist.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    client_id: int
    prefix: str
    label: str | None = None
    status: str
    last_used_at: str | None = None
    created_at: str
    rate_limit_per_minute: int
    ip_whitelist: list[IpWhitelistEntry] = []
    request_ip: str


class TopupInvoiceData(BaseModel):
    """Crypto top-up invoice snapshot.

    Returned by ``POST /account/topup`` (creation) and
    ``GET /account/topup/{invoice_id}`` (status poll). Both endpoints share
    one model because the SDK merges fields across calls — ``payment_url``
    and ``expires_at`` are populated on creation, ``paid_at`` and
    ``balance_after`` on status poll once paid. The pure-data model is
    pydantic-validated; the polling-aware wrapper that wraps it lives in
    :mod:`impreza._topup` (:class:`impreza.TopupInvoice`).

    Status lifecycle:

    * ``pending`` — gateway has not confirmed payment yet (non-terminal)
    * ``paid`` — gateway confirmed; balance credited (terminal success)
    * ``cancelled`` — gateway / admin cancelled before payment (terminal failure)
    * ``refunded`` — payment was reversed (terminal failure)
    """

    model_config = ConfigDict(extra="ignore")

    invoice_id: int
    amount: float
    currency: str
    method: str | None = None
    status: str
    payment_url: str | None = None
    expires_at: str | None = None
    paid_at: str | None = None
    balance_after: float | None = None
