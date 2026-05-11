"""Unit tests for the webhooks layer (Phase 1.6).

Two surfaces to cover:

* **Receiver helpers** in :mod:`impreza.webhooks` — ``verify_signature``,
  ``parse_event``, ``compute_signature``, ``WebhookSignatureMismatch``.
  No HTTP — pure Python verification of a known body / header / secret
  triple.
* **SDK resource** in :mod:`impreza.resources.webhooks` — ``c.webhooks``
  CRUD + rotate-secret + deliveries + event-types. Mocked via respx.
"""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from impreza import (
    AsyncClient,
    Client,
    WebhookDelivery,
    WebhookEvent,
    WebhookEventCatalog,
    WebhookSignatureMismatch,
    WebhookSubscription,
)
from impreza.webhooks import compute_signature, parse_event, verify_signature

BASE = "https://api.imprezahost.com/v1"

SECRET = "a8f3" + "0" * 60  # 64 hex chars — matches what the server emits
ALT_SECRET = "b9e4" + "0" * 60


# ── receiver: verify_signature happy path ─────────────────────────────


def _sample_event_body() -> bytes:
    """A realistic delivery body — same shape WebhookDispatcher::dispatch emits."""
    return json.dumps(
        {
            "id": "evt_a1b2c3d4e5f60718",
            "type": "vps.power_state_changed",
            "created_at": "2026-05-09T15:42:00Z",
            "data": {"service_id": 1234, "previous": "running", "current": "stopped"},
        },
        separators=(",", ":"),
    ).encode("utf-8")


def test_compute_signature_format() -> None:
    body = b"hello"
    sig = compute_signature(body, SECRET)
    assert sig.startswith("sha256=")
    # 64 hex chars after the prefix
    hex_part = sig[len("sha256=") :]
    assert len(hex_part) == 64
    assert all(c in "0123456789abcdef" for c in hex_part)


def test_compute_signature_str_and_bytes_match() -> None:
    body_bytes = b'{"hello":"world"}'
    body_str = '{"hello":"world"}'
    assert compute_signature(body_bytes, SECRET) == compute_signature(body_str, SECRET)


def test_verify_signature_returns_typed_event_on_match() -> None:
    body = _sample_event_body()
    sig = compute_signature(body, SECRET)
    event = verify_signature(body=body, signature_header=sig, secret=SECRET)
    assert isinstance(event, WebhookEvent)
    assert event.id == "evt_a1b2c3d4e5f60718"
    assert event.type == "vps.power_state_changed"
    assert event.data["service_id"] == 1234


def test_verify_signature_accepts_str_body() -> None:
    body = _sample_event_body().decode("utf-8")
    sig = compute_signature(body, SECRET)
    event = verify_signature(body=body, signature_header=sig, secret=SECRET)
    assert event.type == "vps.power_state_changed"


# ── receiver: verify_signature rejection paths ────────────────────────


def test_verify_signature_rejects_tampered_body() -> None:
    body = _sample_event_body()
    sig = compute_signature(body, SECRET)
    tampered = body.replace(b"stopped", b"running")
    with pytest.raises(WebhookSignatureMismatch):
        verify_signature(body=tampered, signature_header=sig, secret=SECRET)


def test_verify_signature_rejects_wrong_secret() -> None:
    body = _sample_event_body()
    sig = compute_signature(body, SECRET)
    with pytest.raises(WebhookSignatureMismatch):
        verify_signature(body=body, signature_header=sig, secret=ALT_SECRET)


def test_verify_signature_rejects_missing_prefix() -> None:
    body = _sample_event_body()
    raw_hex = compute_signature(body, SECRET)[len("sha256=") :]
    with pytest.raises(WebhookSignatureMismatch):
        verify_signature(body=body, signature_header=raw_hex, secret=SECRET)


def test_verify_signature_rejects_empty_header() -> None:
    body = _sample_event_body()
    with pytest.raises(WebhookSignatureMismatch):
        verify_signature(body=body, signature_header="", secret=SECRET)


def test_verify_signature_rejects_unknown_algorithm_prefix() -> None:
    body = _sample_event_body()
    fake_sig = "md5=" + "0" * 32
    with pytest.raises(WebhookSignatureMismatch):
        verify_signature(body=body, signature_header=fake_sig, secret=SECRET)


def test_verify_signature_message_does_not_leak_details() -> None:
    """The exception message is intentionally vague — never reveal which
    half of the comparison failed first or whether the prefix matched."""
    body = _sample_event_body()
    sig_bad = compute_signature(body, ALT_SECRET)
    with pytest.raises(WebhookSignatureMismatch) as exc_info:
        verify_signature(body=body, signature_header=sig_bad, secret=SECRET)
    # Same message regardless of which check tripped:
    assert str(exc_info.value) == "signature mismatch"


# ── parse_event ────────────────────────────────────────────────────────


def test_parse_event_happy_path() -> None:
    event = parse_event(_sample_event_body())
    assert event.type == "vps.power_state_changed"


def test_parse_event_rejects_invalid_json() -> None:
    with pytest.raises(ValueError, match="JSON"):
        parse_event(b"not json")


def test_parse_event_rejects_non_object_root() -> None:
    with pytest.raises(ValueError, match="object"):
        parse_event(b'["not", "an", "object"]')


def test_parse_event_rejects_invalid_utf8_bytes() -> None:
    with pytest.raises(ValueError, match="UTF-8"):
        parse_event(b"\xff\xfe\xfd not utf-8")


# ── SDK resource: list / get / event_types ────────────────────────────


def _ok(data: dict[str, object]) -> dict[str, object]:
    return {"success": True, "data": data, "meta": {"request_id": "req_test"}}


def _subscription_payload(subscription_id: int = 1, secret: str | None = None) -> dict[str, object]:
    payload: dict[str, object] = {
        "id": subscription_id,
        "url": "https://example.com/hooks/impreza",
        "events": ["vps.*", "topup.paid"],
        "description": "production handler",
        "is_active": True,
        "last_delivery_at": None,
        "last_delivery_status": None,
        "created_at": "2026-05-09T16:00:00Z",
    }
    if secret is not None:
        payload["secret"] = secret
        payload["secret_warning"] = "Save this secret — it cannot be retrieved later."
    return payload


@respx.mock
def test_webhooks_list_returns_typed_subscriptions() -> None:
    respx.get(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "webhooks": [_subscription_payload(1), _subscription_payload(2)],
                    "total": 2,
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        subs = c.webhooks.list()
    assert len(subs) == 2
    assert all(isinstance(s, WebhookSubscription) for s in subs)
    assert subs[0].id == 1
    assert subs[0].secret is None  # never on list


@respx.mock
def test_webhooks_get_returns_one_subscription() -> None:
    respx.get(f"{BASE}/webhooks/42").mock(
        return_value=httpx.Response(200, json=_ok(_subscription_payload(42)))
    )
    with Client(api_key="x", api_secret="y") as c:
        sub = c.webhooks.get(42)
    assert isinstance(sub, WebhookSubscription)
    assert sub.id == 42
    assert sub.events == ["vps.*", "topup.paid"]


@respx.mock
def test_webhooks_event_types_returns_catalog() -> None:
    respx.get(f"{BASE}/webhooks/event-types").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "event_types": ["webhook.test", "topup.paid", "vps.power_state_changed"],
                    "wildcards": {"*": "Receive every event.", "vps.*": "VPS events."},
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        catalog = c.webhooks.event_types()
    assert isinstance(catalog, WebhookEventCatalog)
    assert "topup.paid" in catalog.event_types
    assert catalog.wildcards["vps.*"] == "VPS events."


# ── SDK resource: create + secret returned once ───────────────────────


@respx.mock
def test_webhooks_create_returns_subscription_with_secret() -> None:
    sub_payload = _subscription_payload(1, secret="abc123" + "0" * 58)
    route = respx.post(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(201, json=_ok(sub_payload))
    )
    with Client(api_key="x", api_secret="y") as c:
        sub = c.webhooks.create(
            url="https://example.com/hooks/impreza",
            events=["vps.*", "topup.paid"],
            description="production handler",
        )
    assert route.called
    body = json.loads(route.calls.last.request.read())
    assert body["url"] == "https://example.com/hooks/impreza"
    assert body["events"] == ["vps.*", "topup.paid"]
    assert body["description"] == "production handler"
    # Secret only present in this response shape
    assert sub.secret == "abc123" + "0" * 58
    assert sub.secret_warning is not None


def test_webhooks_create_requires_at_least_one_event() -> None:
    """Caller-side guard — `events=[]` raises locally, no HTTP call made."""
    with Client(api_key="x", api_secret="y") as c, pytest.raises(ValueError, match="events"):
        c.webhooks.create(url="https://x.com/h", events=[])


# ── SDK resource: update PATCH ─────────────────────────────────────────


@respx.mock
def test_webhooks_update_sends_only_changed_fields() -> None:
    route = respx.patch(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(200, json=_ok(_subscription_payload(1)))
    )
    with Client(api_key="x", api_secret="y") as c:
        c.webhooks.update(1, is_active=False)
    body = json.loads(route.calls.last.request.read())
    assert body == {"is_active": False}


@respx.mock
def test_webhooks_update_with_multiple_fields() -> None:
    route = respx.patch(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(200, json=_ok(_subscription_payload(1)))
    )
    with Client(api_key="x", api_secret="y") as c:
        c.webhooks.update(
            1, url="https://new.example.com/h", events=["topup.paid"], description="new"
        )
    body = json.loads(route.calls.last.request.read())
    assert set(body.keys()) == {"url", "events", "description"}
    assert body["url"] == "https://new.example.com/h"


def test_webhooks_update_with_no_fields_raises_value_error() -> None:
    """update() with no fields is a programmer error — caught locally."""
    with Client(api_key="x", api_secret="y") as c, pytest.raises(ValueError, match="at least"):
        c.webhooks.update(1)


# ── SDK resource: delete + rotate_secret ──────────────────────────────


@respx.mock
def test_webhooks_delete_makes_delete_call() -> None:
    route = respx.delete(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(200, json=_ok({"id": 1, "deleted": True}))
    )
    with Client(api_key="x", api_secret="y") as c:
        c.webhooks.delete(1)
    assert route.called


@respx.mock
def test_webhooks_rotate_secret_returns_new_secret_string() -> None:
    new_secret = "f1a2" + "0" * 60
    respx.post(f"{BASE}/webhooks/1/rotate-secret").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"id": 1, "secret": new_secret, "secret_warning": "save it"}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        secret = c.webhooks.rotate_secret(1)
    assert secret == new_secret


@respx.mock
def test_webhooks_rotate_secret_returns_empty_when_secret_missing() -> None:
    """Defensive: if the server somehow doesn't include the secret, return ""."""
    respx.post(f"{BASE}/webhooks/1/rotate-secret").mock(
        return_value=httpx.Response(200, json=_ok({"id": 1}))
    )
    with Client(api_key="x", api_secret="y") as c:
        assert c.webhooks.rotate_secret(1) == ""


# ── SDK resource: deliveries ──────────────────────────────────────────


@respx.mock
def test_webhooks_deliveries_returns_typed_list() -> None:
    respx.get(f"{BASE}/webhooks/1/deliveries").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "deliveries": [
                        {
                            "id": 100,
                            "event_type": "webhook.test",
                            "event_id": "evt_a1b2",
                            "attempts": 1,
                            "delivered": True,
                            "delivered_at": "2026-05-09T16:01:00Z",
                            "last_response_code": 200,
                        }
                    ],
                    "total": 1,
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        deliveries = c.webhooks.deliveries(1)
    assert len(deliveries) == 1
    assert isinstance(deliveries[0], WebhookDelivery)
    assert deliveries[0].delivered is True
    assert deliveries[0].last_response_code == 200


# ── async equivalents ────────────────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_webhooks_list_and_create() -> None:
    respx.get(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(200, json=_ok({"webhooks": [], "total": 0}))
    )
    sub_payload = _subscription_payload(99, secret="x" * 64)
    respx.post(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(201, json=_ok(sub_payload))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        assert await c.webhooks.list() == []
        sub = await c.webhooks.create(url="https://x.com", events=["*"])
    assert sub.id == 99
    assert sub.secret == "x" * 64


@pytest.mark.asyncio
@respx.mock
async def test_async_webhooks_rotate_and_delete() -> None:
    respx.post(f"{BASE}/webhooks/5/rotate-secret").mock(
        return_value=httpx.Response(
            200, json=_ok({"id": 5, "secret": "y" * 64, "secret_warning": "save"})
        )
    )
    respx.delete(f"{BASE}/webhooks/5").mock(
        return_value=httpx.Response(200, json=_ok({"id": 5, "deleted": True}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        secret = await c.webhooks.rotate_secret(5)
        await c.webhooks.delete(5)
    assert secret == "y" * 64


@pytest.mark.asyncio
@respx.mock
async def test_async_webhooks_event_types() -> None:
    respx.get(f"{BASE}/webhooks/event-types").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"event_types": ["webhook.test"], "wildcards": {}}),
        )
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        cat = await c.webhooks.event_types()
    assert "webhook.test" in cat.event_types


@pytest.mark.asyncio
@respx.mock
async def test_async_webhooks_get_standalone() -> None:
    """Async parity for ``c.webhooks.get(id)`` — the sync side has
    a standalone get test (test_webhooks_get_returns_subscription),
    the async side only had a list+create combined test."""
    respx.get(f"{BASE}/webhooks/42").mock(
        return_value=httpx.Response(200, json=_ok(_subscription_payload(42)))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        sub = await c.webhooks.get(42)
    assert sub.id == 42


@pytest.mark.asyncio
@respx.mock
async def test_async_webhooks_update() -> None:
    """Async parity for ``c.webhooks.update(id, **kwargs)`` —
    PATCH /webhooks/{id} with one field at a time. Mirrors the
    sync test_webhooks_update_url_only test."""
    route = respx.patch(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(
            200, json=_ok(_subscription_payload(1))
        )
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        await c.webhooks.update(1, url="https://new.example.com/hook")
    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {"url": "https://new.example.com/hook"}


@pytest.mark.asyncio
@respx.mock
async def test_async_webhooks_deliveries() -> None:
    """Async parity for ``c.webhooks.deliveries(id)`` — same
    typed-list extraction as the sync side."""
    respx.get(f"{BASE}/webhooks/1/deliveries").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "deliveries": [
                    {
                        "id": 100,
                        "event_type": "webhook.test",
                        "event_id": "evt_async",
                        "attempts": 2,
                        "delivered": True,
                        "delivered_at": "2026-05-11T12:00:00Z",
                        "last_response_code": 200,
                    },
                ],
                "total": 1,
            }),
        )
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        deliveries = await c.webhooks.deliveries(1)
    assert len(deliveries) == 1
    assert isinstance(deliveries[0], WebhookDelivery)
    assert deliveries[0].event_id == "evt_async"
    assert deliveries[0].last_response_code == 200
