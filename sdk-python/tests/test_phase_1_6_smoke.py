"""Live integration smoke tests for Phase 1.6 (webhooks).

The CRUD round-trip (create → list → rotate → delete) is *not*
destructive in the usual sense — it only touches the client's own
webhook subscriptions, never charges money or alters another resource.
So we run the full lifecycle by default and clean up after ourselves.

The receiver-side helpers (``verify_signature``, ``parse_event``) are
fully unit-tested with no network — there's no live test for them
beyond confirming a real ``c.webhooks.create()`` response carries a
secret of the expected shape.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_6_smoke.py -v -s
"""

from __future__ import annotations

from impreza import Client, WebhookSubscription
from impreza.webhooks import compute_signature

# A throwaway URL that accepts and discards POSTs. The webhook.site URL
# format is `https://webhook.site/<uuid>` — using a fixed one would
# leak signatures to a public log, so we use httpbin.org/anything which
# doesn't persist anything beyond the response.
RECEIVER_URL = "https://httpbin.org/anything"


def test_smoke_webhooks_event_types_lists_known_events(live_client: Client) -> None:
    """``event_types()`` should return a non-empty catalog with at least
    the ``webhook.test`` event the server uses for its own delivery probe."""
    catalog = live_client.webhooks.event_types()
    assert "webhook.test" in catalog.event_types, (
        f"webhook.test not in catalog; got: {catalog.event_types}"
    )
    print(
        f"\n  catalog has {len(catalog.event_types)} event(s); "
        f"{len(catalog.wildcards)} wildcard pattern(s)"
    )


def test_smoke_webhooks_full_lifecycle(live_client: Client) -> None:
    """Create → list → get → rotate-secret → delete on the live server.

    Cleans up the subscription unconditionally (try / finally) so a
    failed assertion partway through doesn't leave debris on the test
    account.
    """
    sub = live_client.webhooks.create(
        url=RECEIVER_URL,
        events=["webhook.test"],
        description="phase-1.6 smoke",
    )
    try:
        assert isinstance(sub, WebhookSubscription)
        assert sub.id > 0
        assert sub.url == RECEIVER_URL
        assert sub.events == ["webhook.test"]
        # Secret is shown ONCE — must be present here, hex-shaped, 64 chars
        assert sub.secret is not None
        assert len(sub.secret) == 64
        assert all(c in "0123456789abcdef" for c in sub.secret)

        # The created subscription should appear in list()
        listed = live_client.webhooks.list()
        ids = [s.id for s in listed]
        assert sub.id in ids, f"subscription {sub.id} missing from list {ids}"

        # get() round-trips the same shape (without the secret)
        fetched = live_client.webhooks.get(sub.id)
        assert fetched.id == sub.id
        assert fetched.url == sub.url
        assert fetched.secret is None  # never returned again

        # rotate_secret() returns a new 64-hex secret
        new_secret = live_client.webhooks.rotate_secret(sub.id)
        assert len(new_secret) == 64
        assert new_secret != sub.secret  # rotated, not echoed

        # We can sign a payload locally with the rotated secret — round-trip
        # the helper code path against a real secret.
        body = b'{"hello":"world"}'
        sig = compute_signature(body, new_secret)
        assert sig.startswith("sha256=")
        assert len(sig) == len("sha256=") + 64

        print(
            f"\n  subscription {sub.id}: created -> listed -> rotated; "
            f"secret length={len(new_secret)}"
        )
    finally:
        live_client.webhooks.delete(sub.id)
        # Confirm it's gone
        ids_after = [s.id for s in live_client.webhooks.list()]
        assert sub.id not in ids_after, (
            f"subscription {sub.id} still in list after delete: {ids_after}"
        )
