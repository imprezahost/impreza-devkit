"""Account resource — accessed via ``Client.account`` and ``AsyncClient.account``.

Sync and async variants live in the same file so the parallel surface
is visible side-by-side.

The account resource owns ``account.services`` as a sub-resource, since
``GET /account/services`` returns services scoped to the authenticated
client. Phase 1.7 adds crypto top-up: ``account.topup()`` returns a
:class:`~impreza.TopupInvoice` future with ``wait_until_paid()`` baked
in, mirroring the ``Operation`` pattern from Phase 1.5.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from .._topup import (
    AsyncTopupInvoice,
    TopupInvoice,
    build_async_topup_invoice,
    build_topup_invoice,
)
from ..models.account import AccountInfo, KeyIdentity
from .services import AsyncServicesResource, ServicesResource

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


_ALLOWED_TOPUP_METHODS = ("btc", "xmr", "trx", "usdt", "usdt_trc20")


def _validate_topup_args(amount: float, method: str | None) -> None:
    """Mirror server-side validation client-side so ``InvalidRequest``
    doesn't have to round-trip for obvious mistakes."""
    if amount < 1 or amount > 10000:
        raise ValueError(
            "amount must be between 1.00 and 10000.00 "
            "(server enforces the same range).",
        )
    if method is not None and method not in _ALLOWED_TOPUP_METHODS:
        raise ValueError(
            f"method must be one of {_ALLOWED_TOPUP_METHODS} or None, "
            f"got: {method!r}.",
        )


def _topup_body(amount: float, method: str | None) -> dict[str, Any]:
    body: dict[str, Any] = {"amount": amount}
    if method is not None:
        body["method"] = method
    return body


class AccountResource:
    """Sync operations on the authenticated client's account."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http
        self.services = ServicesResource(http)

    def get(self) -> AccountInfo:
        """Return the authenticated client's profile and balance.

        Wraps ``GET /account``.
        """
        payload = self._http.get("/account")
        data_raw = payload.get("data")
        data = data_raw if isinstance(data_raw, dict) else {}
        return AccountInfo.model_validate(data)

    def api_key_self(self) -> KeyIdentity:
        """Identity, scopes, and IP whitelist of the API key making
        this request.

        Wraps ``GET /account/api-keys/self``. Returns the public
        ``prefix`` (first 12 chars of the key), the label, status,
        rate limit, IP whitelist, and the ``request_ip`` the server
        observed. The full key value is never exposed — rotate via
        your Impreza Account if the secret has leaked.
        """
        payload = self._http.get("/account/api-keys/self")
        data_raw = payload.get("data")
        data = data_raw if isinstance(data_raw, dict) else {}
        return KeyIdentity.model_validate(data)

    def topup(
        self,
        *,
        amount: float,
        method: str | None = None,
    ) -> TopupInvoice:
        """Create a crypto top-up invoice.

        Wraps ``POST /account/topup``. The server creates an ``AddFunds``
        invoice routed to the ``btcpayinline`` payment gateway and returns
        a :class:`~impreza.TopupInvoice` whose ``payment_url`` is the
        public link a client (or their integration) opens to settle.

        Once the gateway confirms payment, Impreza Account automatically credits
        the client balance. Use ``invoice.wait_until_paid()`` to block
        until that happens, or subscribe to the ``topup.paid`` webhook
        event for a push-driven flow.

        Args:
            amount: amount in the client's account currency. Must be
                between 1.00 and 10000.00.
            method: optional payment method hint. One of ``btc``, ``xmr``,
                ``trx``, ``usdt``, ``usdt_trc20``. The gateway may show
                alternatives if the preferred method is unavailable.

        Raises:
            ValueError: when ``amount`` is out of range or ``method`` is
                not one of the allowed values.
        """
        _validate_topup_args(amount, method)
        payload = self._http.post("/account/topup", json=_topup_body(amount, method))
        return build_topup_invoice(self._http, payload)

    def topup_status(self, invoice_id: int) -> TopupInvoice:
        """Fetch the current status of a top-up invoice.

        Wraps ``GET /account/topup/{invoice_id}``. Returns a
        :class:`~impreza.TopupInvoice` whose ``status`` reflects the
        latest gateway state. Note that ``payment_url`` and
        ``expires_at`` are not echoed by this endpoint — they are only
        set on the original ``topup()`` call.
        """
        payload = self._http.get(f"/account/topup/{invoice_id}")
        return build_topup_invoice(self._http, payload)


class AsyncAccountResource:
    """Async operations on the authenticated client's account."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http
        self.services = AsyncServicesResource(http)

    async def get(self) -> AccountInfo:
        payload = await self._http.get("/account")
        data_raw = payload.get("data")
        data = data_raw if isinstance(data_raw, dict) else {}
        return AccountInfo.model_validate(data)

    async def api_key_self(self) -> KeyIdentity:
        """Async counterpart of :meth:`AccountResource.api_key_self`."""
        payload = await self._http.get("/account/api-keys/self")
        data_raw = payload.get("data")
        data = data_raw if isinstance(data_raw, dict) else {}
        return KeyIdentity.model_validate(data)

    async def topup(
        self,
        *,
        amount: float,
        method: str | None = None,
    ) -> AsyncTopupInvoice:
        """Async counterpart of :meth:`AccountResource.topup`."""
        _validate_topup_args(amount, method)
        payload = await self._http.post(
            "/account/topup",
            json=_topup_body(amount, method),
        )
        return build_async_topup_invoice(self._http, payload)

    async def topup_status(self, invoice_id: int) -> AsyncTopupInvoice:
        """Async counterpart of :meth:`AccountResource.topup_status`."""
        payload = await self._http.get(f"/account/topup/{invoice_id}")
        return build_async_topup_invoice(self._http, payload)
