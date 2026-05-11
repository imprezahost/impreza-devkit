"""Webhook-related response models (Phase 1.6).

The server emits HMAC-signed JSON payloads with a stable envelope:

    {
        "id":         "evt_a1b2c3d4...",
        "type":       "vps.power_state_changed",
        "created_at": "2026-05-09T15:42:00Z",
        "data":       { event-specific fields }
    }

``WebhookEvent`` decodes that envelope. ``WebhookSubscription`` and
``WebhookDelivery`` decode the corresponding management payloads on
the SDK side (``c.webhooks``).
"""

from __future__ import annotations

from typing import Any

from pydantic import BaseModel, ConfigDict, Field


class WebhookEvent(BaseModel):
    """A signed event delivered by the server to a subscription URL.

    Returned by :func:`impreza.webhooks.verify_signature` and
    :func:`impreza.webhooks.parse_event`. Use ``event.type`` to dispatch
    on the event kind and ``event.data`` to read the event-specific
    payload (its shape varies per event type).

    Unknown fields on the wire are silently ignored — forward-compatible
    when the server adds new envelope fields.
    """

    model_config = ConfigDict(extra="ignore")

    id: str
    type: str
    created_at: str
    data: dict[str, Any] = Field(default_factory=dict)


class WebhookSubscription(BaseModel):
    """A webhook subscription owned by the authenticated client.

    Returned by ``c.webhooks.list()`` / ``.get()`` / ``.create()`` /
    ``.update()``. The ``secret`` field is only populated on the response
    of ``.create()`` and ``.rotate_secret()`` — every other call returns
    ``None`` for it (the server doesn't store the plaintext key for
    return).
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    url: str
    events: list[str] = Field(default_factory=list)
    description: str = ""
    is_active: bool = True
    last_delivery_at: str | None = None
    last_delivery_status: int | None = None
    created_at: str | None = None

    # Populated only on `create` and `rotate_secret` responses
    secret: str | None = None
    secret_warning: str | None = None


class WebhookDelivery(BaseModel):
    """A single delivery attempt history record.

    Returned (in lists of up to 100) by
    ``c.webhooks.deliveries(subscription_id)``.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    event_type: str
    event_id: str
    attempts: int = 0
    next_attempt_at: str | None = None
    last_attempted_at: str | None = None
    last_response_code: int | None = None
    last_error: str | None = None
    delivered: bool = False
    delivered_at: str | None = None
    created_at: str | None = None


class WebhookEventCatalog(BaseModel):
    """The event-type catalog returned by ``c.webhooks.event_types()``.

    ``event_types`` is the list of concrete events the server can emit;
    ``wildcards`` is a dict of supported wildcard patterns to their
    description (e.g. ``{"vps.*": "Receive every event whose type starts
    with vps."}``).
    """

    model_config = ConfigDict(extra="ignore")

    event_types: list[str] = Field(default_factory=list)
    wildcards: dict[str, str] = Field(default_factory=dict)
