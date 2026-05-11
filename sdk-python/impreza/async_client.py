"""Public async client.

Usage::

    import asyncio
    from impreza import AsyncClient

    async def main():
        async with AsyncClient.from_env() as c:
            print((await c.account.get()).balance)

    asyncio.run(main())

Tor routing works the same way as the sync :class:`Client`::

    async with AsyncClient.from_env(use_tor=True) as c:
        ...
    # Or with IMPREZA_USE_TOR=1 in the environment.

    async with AsyncClient.from_env(auto_tor=True) as c:
        ...  # Falls back to clearnet if Tor is not running.
"""

from __future__ import annotations

import os
from types import TracebackType
from typing import Any

from ._http import DEFAULT_BASE_URL, DEFAULT_MAX_RETRIES, DEFAULT_TIMEOUT
from ._http_async import AsyncHttpClient
from ._tor import resolve_proxy
from .resources.account import AsyncAccountResource
from .resources.catalog import AsyncCatalogResource
from .resources.domains import AsyncDomainsResource
from .resources.email import AsyncEmailResource
from .resources.hosting import AsyncHostingResource
from .resources.invoices import AsyncInvoicesResource
from .resources.orders import AsyncOrdersResource
from .resources.vps import AsyncVpsResource
from .resources.webhooks import AsyncWebhooksResource


class AsyncClient:
    """Async client for the Impreza Host public API.

    Mirrors the resource surface of :class:`Client`, but every operation
    is a coroutine. Backed by ``httpx.AsyncClient`` and shares the same
    error mapping, retry policy, and Tor proxy resolution.
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
        self._http = AsyncHttpClient(
            api_key=api_key,
            api_secret=api_secret,
            base_url=base_url,
            timeout=timeout,
            max_retries=max_retries,
            proxy=resolved_proxy,
        )

        # Resources
        self.account = AsyncAccountResource(self._http)
        self.catalog = AsyncCatalogResource(self._http)
        self.domains = AsyncDomainsResource(self._http)
        self.email = AsyncEmailResource(self._http)
        self.hosting = AsyncHostingResource(self._http)
        self.invoices = AsyncInvoicesResource(self._http)
        self.orders = AsyncOrdersResource(self._http)
        self.vps = AsyncVpsResource(self._http)
        self.webhooks = AsyncWebhooksResource(self._http)

    @classmethod
    def from_env(cls, **overrides: Any) -> AsyncClient:
        """Construct an AsyncClient from ``IMPREZA_*`` environment variables.

        Reads ``IMPREZA_API_KEY`` and ``IMPREZA_API_SECRET`` (required) and
        ``IMPREZA_API_BASE`` (optional). ``IMPREZA_USE_TOR=1`` toggles Tor
        routing transparently.

        Raises:
            RuntimeError: when the required env variables are not set.
        """
        api_key = os.environ.get("IMPREZA_API_KEY")
        api_secret = os.environ.get("IMPREZA_API_SECRET")
        if not api_key or not api_secret:
            raise RuntimeError(
                "Missing IMPREZA_API_KEY or IMPREZA_API_SECRET in environment. "
                "Either set them or pass api_key/api_secret to AsyncClient(...) directly."
            )

        base_url = os.environ.get("IMPREZA_API_BASE", DEFAULT_BASE_URL)
        kwargs: dict[str, Any] = {
            "api_key": api_key,
            "api_secret": api_secret,
            "base_url": base_url,
        }
        kwargs.update(overrides)
        return cls(**kwargs)

    async def aclose(self) -> None:
        await self._http.aclose()

    async def __aenter__(self) -> AsyncClient:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        await self.aclose()
