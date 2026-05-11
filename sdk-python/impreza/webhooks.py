"""Webhook receiver helpers (Phase 1.6).

This module is consumed by **applications that receive webhooks from
Impreza** — Flask / FastAPI / Django apps, queue workers, etc. It is
*not* the SDK resource for managing webhook subscriptions; that lives
at :class:`impreza.resources.webhooks.WebhooksResource` and is reached
via ``client.webhooks``.

Typical receiver code::

    from impreza.webhooks import verify_signature, WebhookSignatureMismatch

    @app.post("/hooks/impreza")
    def receive():
        try:
            event = verify_signature(
                body=request.body,
                signature_header=request.headers.get("X-Impreza-Signature", ""),
                secret=os.environ["IMPREZA_WEBHOOK_SECRET"],
            )
        except WebhookSignatureMismatch:
            return ("invalid signature", 401)

        if event.type == "vps.power_state_changed":
            handle_power_change(event.data)
        return ("ok", 200)

The signature header format is ``sha256=<hex>`` (HMAC-SHA256 of the
**raw** request body using the subscription's secret as key).
Comparison is timing-safe via :func:`hmac.compare_digest`.

Use :func:`parse_event` only when you have already verified the
signature elsewhere or when running a **local** test fixture — never
trust a payload whose signature has not been verified.
"""

from __future__ import annotations

import hmac
import json
from hashlib import sha256
from typing import Any

from .exceptions import WebhookSignatureMismatch
from .models.webhook import WebhookEvent

_SIGNATURE_PREFIX = "sha256="


def compute_signature(body: bytes | str, secret: str) -> str:
    """Compute the HMAC-SHA256 signature of ``body`` keyed by ``secret``.

    Returns a string in the same format the server emits in the
    ``X-Impreza-Signature`` header: ``"sha256=<hex>"``. Useful for
    testing receiver code with mocked deliveries.
    """
    body_bytes = body if isinstance(body, (bytes, bytearray)) else body.encode("utf-8")
    digest = hmac.new(secret.encode("utf-8"), body_bytes, sha256).hexdigest()
    return f"{_SIGNATURE_PREFIX}{digest}"


def verify_signature(
    *,
    body: bytes | str,
    signature_header: str,
    secret: str,
) -> WebhookEvent:
    """Verify the signature on an incoming webhook delivery and parse the body.

    Args:
        body: the **raw** HTTP request body, exactly as the server sent it.
            Bytes are preferred; strings are encoded as UTF-8 if passed.
            Re-serializing JSON before signing breaks the signature, so
            always work from the unparsed body.
        signature_header: the value of the ``X-Impreza-Signature`` header
            on the incoming request. Expected format: ``"sha256=<hex>"``.
        secret: the subscription's HMAC secret, returned by
            ``c.webhooks.create()`` once and stored by the receiver.

    Returns:
        The parsed :class:`WebhookEvent`.

    Raises:
        WebhookSignatureMismatch: if the signature header is malformed,
            uses an unknown algorithm prefix, or the computed digest
            does not match (timing-safe comparison).
        ValueError: if the body is not valid JSON, or the JSON object
            does not match the :class:`WebhookEvent` shape.
    """
    if not isinstance(signature_header, str) or not signature_header.startswith(
        _SIGNATURE_PREFIX
    ):
        raise WebhookSignatureMismatch("signature mismatch")

    body_bytes = body if isinstance(body, (bytes, bytearray)) else body.encode("utf-8")
    expected = compute_signature(body_bytes, secret)

    # Timing-safe compare, normalized to bytes
    if not hmac.compare_digest(expected.encode("ascii"), signature_header.encode("ascii")):
        raise WebhookSignatureMismatch("signature mismatch")

    return parse_event(body_bytes)


def parse_event(body: bytes | str) -> WebhookEvent:
    """Parse a JSON webhook body into a :class:`WebhookEvent`.

    Does **not** verify the signature. Only call this directly when:

    * you already verified the signature elsewhere
      (e.g. at a reverse proxy that re-signs and relays the call), or
    * you're constructing a fixture in a local test.

    For incoming network traffic, always use :func:`verify_signature`.

    Raises:
        ValueError: when the body is not valid JSON or doesn't match
            the event envelope shape.
    """
    if isinstance(body, (bytes, bytearray)):
        try:
            payload: Any = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ValueError(f"webhook body is not valid UTF-8 JSON: {exc}") from exc
    else:
        try:
            payload = json.loads(body)
        except json.JSONDecodeError as exc:
            raise ValueError(f"webhook body is not valid JSON: {exc}") from exc

    if not isinstance(payload, dict):
        raise ValueError("webhook body must be a JSON object")

    return WebhookEvent.model_validate(payload)


__all__ = [
    "WebhookEvent",
    "WebhookSignatureMismatch",
    "compute_signature",
    "parse_event",
    "verify_signature",
]
