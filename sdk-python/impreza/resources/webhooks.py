"""Webhooks subscription management — accessed via ``Client.webhooks`` and
``AsyncClient.webhooks`` (Phase 1.6).

This is the **SDK side** of webhooks: list, create, update, rotate, and
delete subscriptions on the authenticated client's account, and inspect
the delivery history per subscription. The **receiver-side** verification
helpers live at :mod:`impreza.webhooks` (top-level module, not a
resource).

Phase 0 already shipped the seven endpoints this resource wraps:

* ``GET    /webhooks``
* ``POST   /webhooks``
* ``GET    /webhooks/event-types``
* ``GET    /webhooks/{id}``
* ``PATCH  /webhooks/{id}``
* ``DELETE /webhooks/{id}``
* ``POST   /webhooks/{id}/rotate-secret``
* ``GET    /webhooks/{id}/deliveries``

The HMAC ``secret`` is shown ONCE at creation and again on rotate. Every
other call returns ``None`` for the ``secret`` field.
"""

from __future__ import annotations

import builtins
from typing import TYPE_CHECKING

from ..models.webhook import (
    WebhookDelivery,
    WebhookEventCatalog,
    WebhookSubscription,
)

# `WebhooksResource` and `AsyncWebhooksResource` define a `list()` method,
# which shadows the builtin `list` within the class body for mypy's
# class-scope name resolution (PEP 563 defers evaluation but mypy still
# resolves names per Python scoping rules during type-checking). Using
# `builtins.list[X]` inside those classes is unambiguous.

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


# ── extractors / body builders (shared) ────────────────────────────────


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _extract_subscription(payload: dict[str, object]) -> WebhookSubscription:
    return WebhookSubscription.model_validate(_data(payload))


def _extract_subscriptions(payload: dict[str, object]) -> list[WebhookSubscription]:
    data = _data(payload)
    raw = data.get("webhooks")
    if not isinstance(raw, list):
        return []
    return [WebhookSubscription.model_validate(item) for item in raw if isinstance(item, dict)]


def _extract_deliveries(payload: dict[str, object]) -> list[WebhookDelivery]:
    data = _data(payload)
    raw = data.get("deliveries")
    if not isinstance(raw, list):
        return []
    return [WebhookDelivery.model_validate(item) for item in raw if isinstance(item, dict)]


def _extract_catalog(payload: dict[str, object]) -> WebhookEventCatalog:
    return WebhookEventCatalog.model_validate(_data(payload))


def _extract_secret(payload: dict[str, object]) -> str:
    """Pull the rotated secret out of the rotate-secret response."""
    data = _data(payload)
    secret = data.get("secret")
    if not isinstance(secret, str) or not secret:
        return ""
    return secret


def _create_body(
    *,
    url: str,
    events: list[str],
    description: str | None,
) -> dict[str, object]:
    if not events:
        raise ValueError("events must contain at least one event type or wildcard")
    body: dict[str, object] = {"url": url, "events": list(events)}
    if description is not None:
        body["description"] = description
    return body


def _update_body(
    *,
    url: str | None,
    events: list[str] | None,
    description: str | None,
    is_active: bool | None,
) -> dict[str, object]:
    body: dict[str, object] = {}
    if url is not None:
        body["url"] = url
    if events is not None:
        body["events"] = list(events)
    if description is not None:
        body["description"] = description
    if is_active is not None:
        body["is_active"] = bool(is_active)
    if not body:
        raise ValueError("update() requires at least one of url, events, description, is_active")
    return body


# ── sync ───────────────────────────────────────────────────────────────


class WebhooksResource:
    """Sync access to the authenticated client's webhook subscriptions."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self) -> list[WebhookSubscription]:
        """Return every subscription the client owns."""
        return _extract_subscriptions(self._http.get("/webhooks"))

    def get(self, subscription_id: int) -> WebhookSubscription:
        """Return one subscription by ID."""
        return _extract_subscription(self._http.get(f"/webhooks/{subscription_id}"))

    def create(
        self,
        *,
        url: str,
        events: builtins.list[str],
        description: str | None = None,
    ) -> WebhookSubscription:
        """Create a new subscription and return it (with ``secret`` populated).

        The returned subscription's ``secret`` field is the HMAC-SHA256 key
        the server will use to sign deliveries — store it securely. It will
        not be returned by any subsequent call; rotate it instead with
        :meth:`rotate_secret` if it leaks.

        ``events`` accepts concrete event types (``"topup.paid"``) and
        wildcards (``"vps.*"``, ``"*"``).
        """
        return _extract_subscription(
            self._http.post(
                "/webhooks",
                json=_create_body(url=url, events=events, description=description),
            )
        )

    def update(
        self,
        subscription_id: int,
        *,
        url: str | None = None,
        events: builtins.list[str] | None = None,
        description: str | None = None,
        is_active: bool | None = None,
    ) -> WebhookSubscription:
        """Update one or more fields on the subscription. PATCH semantics —
        unspecified fields are left alone."""
        return _extract_subscription(
            self._http.patch(
                f"/webhooks/{subscription_id}",
                json=_update_body(
                    url=url,
                    events=events,
                    description=description,
                    is_active=is_active,
                ),
            )
        )

    def delete(self, subscription_id: int) -> None:
        """Delete the subscription. Pending undelivered events are dropped."""
        self._http.delete(f"/webhooks/{subscription_id}")

    def rotate_secret(self, subscription_id: int) -> str:
        """Generate a fresh HMAC secret and return it.

        The previous secret stops working immediately. The new secret is
        only returned by this call — store it before the response goes
        out of scope.
        """
        return _extract_secret(
            self._http.post(f"/webhooks/{subscription_id}/rotate-secret")
        )

    def deliveries(self, subscription_id: int) -> builtins.list[WebhookDelivery]:
        """List recent delivery attempts (up to 100) for the subscription."""
        return _extract_deliveries(
            self._http.get(f"/webhooks/{subscription_id}/deliveries")
        )

    def event_types(self) -> WebhookEventCatalog:
        """Return the catalog of subscribable event types and wildcards.

        Use this to discover which event names a receiver can subscribe
        to. The list grows as the server adds events; the SDK does not
        hardcode a copy.
        """
        return _extract_catalog(self._http.get("/webhooks/event-types"))


# ── async ──────────────────────────────────────────────────────────────


class AsyncWebhooksResource:
    """Async counterpart to :class:`WebhooksResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self) -> list[WebhookSubscription]:
        return _extract_subscriptions(await self._http.get("/webhooks"))

    async def get(self, subscription_id: int) -> WebhookSubscription:
        return _extract_subscription(
            await self._http.get(f"/webhooks/{subscription_id}")
        )

    async def create(
        self,
        *,
        url: str,
        events: builtins.list[str],
        description: str | None = None,
    ) -> WebhookSubscription:
        return _extract_subscription(
            await self._http.post(
                "/webhooks",
                json=_create_body(url=url, events=events, description=description),
            )
        )

    async def update(
        self,
        subscription_id: int,
        *,
        url: str | None = None,
        events: builtins.list[str] | None = None,
        description: str | None = None,
        is_active: bool | None = None,
    ) -> WebhookSubscription:
        return _extract_subscription(
            await self._http.patch(
                f"/webhooks/{subscription_id}",
                json=_update_body(
                    url=url,
                    events=events,
                    description=description,
                    is_active=is_active,
                ),
            )
        )

    async def delete(self, subscription_id: int) -> None:
        await self._http.delete(f"/webhooks/{subscription_id}")

    async def rotate_secret(self, subscription_id: int) -> str:
        return _extract_secret(
            await self._http.post(f"/webhooks/{subscription_id}/rotate-secret")
        )

    async def deliveries(self, subscription_id: int) -> builtins.list[WebhookDelivery]:
        return _extract_deliveries(
            await self._http.get(f"/webhooks/{subscription_id}/deliveries")
        )

    async def event_types(self) -> WebhookEventCatalog:
        return _extract_catalog(await self._http.get("/webhooks/event-types"))
