"""Public sync client.

Usage::

    from impreza import Client

    with Client.from_env() as c:
        print(c.account.get().balance)

Or pass credentials explicitly::

    c = Client(api_key="imp_...", api_secret="...")

Tor routing::

    # Force Tor:
    c = Client.from_env(use_tor=True)
    # Or with the env var: IMPREZA_USE_TOR=1

    # Probe Tor, fall back to clearnet if not running:
    c = Client.from_env(auto_tor=True)
"""

from __future__ import annotations

import os
from types import TracebackType
from typing import Any

from ._http import (
    DEFAULT_BASE_URL,
    DEFAULT_MAX_RETRIES,
    DEFAULT_TIMEOUT,
    HttpClient,
)
from ._tor import resolve_proxy
from .resources.account import AccountResource
from .resources.catalog import CatalogResource
from .resources.domains import DomainsResource
from .resources.email import EmailResource
from .resources.hosting import HostingResource
from .resources.invoices import InvoicesResource
from .resources.orders import OrdersResource
from .resources.vps import VpsResource
from .resources.webhooks import WebhooksResource


class Client:
    """Sync client for the Impreza Host public API.

    Phase 1.6 adds ``webhooks`` (subscription CRUD + delivery history)
    on top of the 1.5 surface (``account``, ``catalog``, ``domains``,
    ``email``, ``hosting``, ``invoices``, ``orders``, ``vps`` plus the
    ``Operation`` futures returned by long-running VPS ops).

    Receiver-side webhook helpers (``verify_signature``, ``parse_event``)
    live in :mod:`impreza.webhooks` — separate from the SDK resource
    because they are consumed by *applications receiving* webhooks, not
    by code making API calls.

    Tor routing is selected via :func:`impreza._tor.resolve_proxy` —
    explicit ``proxy=`` wins, then ``use_tor`` / ``IMPREZA_USE_TOR``,
    then ``auto_tor`` (probed). Phase 1.2.
    """

    def __init__(
        self,
        api_key: str,
        api_secret: str,
        *,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = DEFAULT_TIMEOUT,
        max_retries: int = DEFAULT_MAX_RETRIES,
        proxy: str | None = None,
        use_tor: bool = False,
        auto_tor: bool = False,
    ) -> None:
        resolved_proxy = resolve_proxy(proxy, use_tor=use_tor, auto_tor=auto_tor)
        self._http = HttpClient(
            api_key=api_key,
            api_secret=api_secret,
            base_url=base_url,
            timeout=timeout,
            max_retries=max_retries,
            proxy=resolved_proxy,
        )

        # Resources
        self.account = AccountResource(self._http)
        self.catalog = CatalogResource(self._http)
        self.domains = DomainsResource(self._http)
        self.email = EmailResource(self._http)
        self.hosting = HostingResource(self._http)
        self.invoices = InvoicesResource(self._http)
        self.orders = OrdersResource(self._http)
        self.vps = VpsResource(self._http)
        self.webhooks = WebhooksResource(self._http)

    @classmethod
    def from_env(cls, **overrides: Any) -> Client:
        """Construct a Client from ``IMPREZA_*`` environment variables.

        Reads ``IMPREZA_API_KEY`` and ``IMPREZA_API_SECRET`` (required) and
        ``IMPREZA_API_BASE`` (optional, defaults to production). Any keyword
        argument override takes precedence over env values::

            c = Client.from_env(timeout=60)

        ``IMPREZA_USE_TOR=1`` in the environment toggles Tor routing — the
        :func:`._tor.resolve_proxy` helper picks that up automatically; you
        do not need to forward it through this method.

        Raises:
            RuntimeError: when the required env variables are not set.
        """
        api_key = os.environ.get("IMPREZA_API_KEY")
        api_secret = os.environ.get("IMPREZA_API_SECRET")
        if not api_key or not api_secret:
            raise RuntimeError(
                "Missing IMPREZA_API_KEY or IMPREZA_API_SECRET in environment. "
                "Either set them or pass api_key/api_secret to Client(...) directly."
            )

        base_url = os.environ.get("IMPREZA_API_BASE", DEFAULT_BASE_URL)
        kwargs: dict[str, Any] = {
            "api_key": api_key,
            "api_secret": api_secret,
            "base_url": base_url,
        }
        kwargs.update(overrides)
        return cls(**kwargs)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self.close()
