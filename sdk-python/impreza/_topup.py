"""Top-up invoice polling — Phase 1.7.

Wraps ``GET /account/topup/{invoice_id}`` in a Future-style object so
callers can write::

    invoice = c.account.topup(amount=50, method="xmr")
    print(invoice.payment_url)               # btcpayinline URL
    invoice.wait_until_paid(timeout=7200)    # 2h default
    print(invoice.balance_after)             # already credited

Or async::

    invoice = await c.account.topup(amount=50, method="xmr")
    await invoice.wait_until_paid(timeout=7200)

The model snapshot is updated in-place on every refresh, so the public
attributes (``status``, ``paid_at``, ``balance_after``) always reflect
the last fetch. Fields populated on the create call (``payment_url``,
``expires_at``) are preserved through ``refresh()`` — the status
endpoint doesn't return them, but they remain useful for retry / display.

A failed terminal state (``cancelled`` / ``refunded``) raises
:class:`~impreza.exceptions.TopupFailed` out of ``.wait_until_paid()``.
A timeout raises :class:`~impreza.exceptions.TopupTimeout`. For silent /
manual handling, use ``invoice.is_paid()`` / ``invoice.is_pending()`` /
``invoice.is_failed()`` after each ``invoice.refresh()`` instead.
"""

from __future__ import annotations

import asyncio
import time
from typing import TYPE_CHECKING

from .exceptions import ApiError, TopupFailed, TopupTimeout
from .models.account import TopupInvoiceData

if TYPE_CHECKING:  # pragma: no cover
    from ._http import HttpClient
    from ._http_async import AsyncHttpClient


_TERMINAL_PAID = {"paid"}
_TERMINAL_FAILED = {"cancelled", "canceled", "refunded", "expired"}

# Crypto confirmations are inherently slow (BTC ~10min, XMR ~2min,
# TRX/USDT-TRC20 ~3s but gateway batching adds latency). 30s strikes a
# balance between responsiveness and not hammering the API.
DEFAULT_POLL_INTERVAL_SECONDS = 30.0

# Matches server-side invoice expiry (gmdate(... + 7200) in
# AccountController::topup). Past this, the invoice is effectively dead
# even if the SDK is still polling.
DEFAULT_TIMEOUT_SECONDS = 7200.0


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _topup_from_payload(payload: dict[str, object]) -> TopupInvoiceData:
    """Build a :class:`TopupInvoiceData` from a server response.

    Raises :class:`ApiError` if the payload is missing the identifiers
    the polling layer needs (``invoice_id`` + ``status``).
    """
    data = _data(payload)
    invoice_id = data.get("invoice_id")
    status = data.get("status")
    if not isinstance(invoice_id, int):
        raise ApiError(
            "Expected a top-up response with an `invoice_id` field, "
            f"got keys: {sorted(data.keys())}.",
        )
    if not isinstance(status, str):
        raise ApiError(
            "Expected `status` to be a string in top-up response, "
            f"got: {type(status).__name__}.",
        )
    return TopupInvoiceData.model_validate(data)


class _TopupInvoiceBase:
    """Shared state + status helpers for sync and async top-up futures.

    Sync and async variants only differ in their ``refresh`` and
    ``wait_until_paid`` IO; data-access and predicates live here.
    """

    __slots__ = ("_snapshot",)

    def __init__(self, snapshot: TopupInvoiceData) -> None:
        self._snapshot = snapshot

    # ── identity & wire data ───────────────────────────────────────────

    @property
    def invoice_id(self) -> int:
        return self._snapshot.invoice_id

    @property
    def amount(self) -> float:
        return self._snapshot.amount

    @property
    def currency(self) -> str:
        return self._snapshot.currency

    @property
    def method(self) -> str | None:
        return self._snapshot.method

    @property
    def status(self) -> str:
        return self._snapshot.status

    @property
    def payment_url(self) -> str | None:
        """Public payment URL (btcpayinline). Set on creation; None when
        only a status poll has been made."""
        return self._snapshot.payment_url

    @property
    def expires_at(self) -> str | None:
        """Invoice expiry timestamp (ISO 8601 UTC). Set on creation."""
        return self._snapshot.expires_at

    @property
    def paid_at(self) -> str | None:
        """Settlement timestamp. Set once the gateway confirms payment."""
        return self._snapshot.paid_at

    @property
    def balance_after(self) -> float | None:
        """Account balance after this credit. Set once status is ``paid``."""
        return self._snapshot.balance_after

    @property
    def snapshot(self) -> TopupInvoiceData:
        """Return the underlying :class:`TopupInvoiceData` (last refresh)."""
        return self._snapshot

    # ── status predicates ──────────────────────────────────────────────

    def is_done(self) -> bool:
        normalized = self.status.lower()
        return normalized in _TERMINAL_PAID or normalized in _TERMINAL_FAILED

    def is_paid(self) -> bool:
        return self.status.lower() in _TERMINAL_PAID

    def is_pending(self) -> bool:
        return not self.is_done()

    def is_failed(self) -> bool:
        return self.status.lower() in _TERMINAL_FAILED

    # ── repr ───────────────────────────────────────────────────────────

    def __repr__(self) -> str:
        cls = type(self).__name__
        return (
            f"{cls}(invoice_id={self.invoice_id}, status={self.status!r}, "
            f"amount={self.amount} {self.currency})"
        )

    # ── internal: merge a status-poll snapshot into the existing one ──

    def _merge_status(self, fresh: TopupInvoiceData) -> None:
        """Update mutable fields from a status poll response.

        ``GET /account/topup/{id}`` doesn't echo ``payment_url`` or
        ``expires_at`` (those are set once on creation). Carry them
        forward from the existing snapshot so a created-then-refreshed
        invoice still has its payment URL.
        """
        merged = fresh.model_copy(
            update={
                "payment_url": fresh.payment_url or self._snapshot.payment_url,
                "expires_at": fresh.expires_at or self._snapshot.expires_at,
            }
        )
        self._snapshot = merged


class TopupInvoice(_TopupInvoiceBase):
    """Sync future wrapping a crypto top-up invoice."""

    __slots__ = ("_http",)

    def __init__(
        self,
        http: HttpClient,
        snapshot: TopupInvoiceData,
    ) -> None:
        super().__init__(snapshot)
        self._http = http

    def refresh(self) -> TopupInvoice:
        """Re-fetch the invoice status. Returns ``self`` for chaining."""
        payload = self._http.get(f"/account/topup/{self.invoice_id}")
        self._merge_status(TopupInvoiceData.model_validate(_data(payload)))
        return self

    def wait_until_paid(
        self,
        *,
        timeout: float | None = DEFAULT_TIMEOUT_SECONDS,
        poll_interval: float = DEFAULT_POLL_INTERVAL_SECONDS,
    ) -> TopupInvoice:
        """Block until the invoice reaches a terminal state.

        Args:
            timeout: maximum wall-clock seconds to block. ``None`` means
                wait indefinitely. Default: 2 hours (matches server-side
                invoice expiry).
            poll_interval: seconds between status polls. Default: 30s
                (crypto confirmations are slow; polling faster wastes
                rate-limit budget).

        Returns ``self`` on success.

        Raises:
            TopupTimeout: when ``timeout`` elapses without a terminal
                state. ``invoice.refresh()`` can be called manually
                afterwards to keep polling.
            TopupFailed: when the invoice ends in
                ``cancelled`` / ``refunded`` / ``expired`` state.
        """
        if poll_interval <= 0:
            raise ValueError("poll_interval must be positive")

        deadline = time.monotonic() + timeout if timeout is not None else None

        while not self.is_done():
            if deadline is not None and time.monotonic() >= deadline:
                raise TopupTimeout(self, timeout or 0.0)
            time.sleep(poll_interval)
            self.refresh()

        if self.is_failed():
            raise TopupFailed(self)
        return self


class AsyncTopupInvoice(_TopupInvoiceBase):
    """Async counterpart to :class:`TopupInvoice`."""

    __slots__ = ("_http",)

    def __init__(
        self,
        http: AsyncHttpClient,
        snapshot: TopupInvoiceData,
    ) -> None:
        super().__init__(snapshot)
        self._http = http

    async def refresh(self) -> AsyncTopupInvoice:
        payload = await self._http.get(f"/account/topup/{self.invoice_id}")
        self._merge_status(TopupInvoiceData.model_validate(_data(payload)))
        return self

    async def wait_until_paid(
        self,
        *,
        timeout: float | None = DEFAULT_TIMEOUT_SECONDS,
        poll_interval: float = DEFAULT_POLL_INTERVAL_SECONDS,
    ) -> AsyncTopupInvoice:
        if poll_interval <= 0:
            raise ValueError("poll_interval must be positive")

        deadline = time.monotonic() + timeout if timeout is not None else None

        while not self.is_done():
            if deadline is not None and time.monotonic() >= deadline:
                raise TopupTimeout(self, timeout or 0.0)
            await asyncio.sleep(poll_interval)
            await self.refresh()

        if self.is_failed():
            raise TopupFailed(self)
        return self


# ── public factory helpers ─────────────────────────────────────────────


def build_topup_invoice(
    http: HttpClient,
    payload: dict[str, object],
) -> TopupInvoice:
    """Build a :class:`TopupInvoice` from a server response payload."""
    return TopupInvoice(http, _topup_from_payload(payload))


def build_async_topup_invoice(
    http: AsyncHttpClient,
    payload: dict[str, object],
) -> AsyncTopupInvoice:
    """Build an :class:`AsyncTopupInvoice` from a server response payload."""
    return AsyncTopupInvoice(http, _topup_from_payload(payload))


__all__ = [
    "AsyncTopupInvoice",
    "TopupInvoice",
    "build_async_topup_invoice",
    "build_topup_invoice",
]
