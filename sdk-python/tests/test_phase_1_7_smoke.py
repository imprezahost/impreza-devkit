"""Live integration smoke tests for Phase 1.7 (crypto top-up).

The default suite is **read-only**: it pulls the current account balance
and validates that ``topup_status()`` decodes correctly when given an
existing invoice ID via the ``IMPREZA_TOPUP_INVOICE_ID`` env var. If
that env var isn't set, the status test skips silently — no destructive
side effects.

The full create-then-status round-trip is gated behind
``IMPREZA_DESTRUCTIVE_TESTS=1`` because ``POST /account/topup`` creates
a real invoice. No money is moved (the invoice stays unpaid until
someone settles it on the gateway), but the invoice does persist in
the API and counts against any per-day limits — opt-in only.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_7_smoke.py -v -s

For the destructive create cycle::

    export IMPREZA_DESTRUCTIVE_TESTS=1
    pytest tests/test_phase_1_7_smoke.py -v -s
"""

from __future__ import annotations

import os

import pytest

from impreza import Client, TopupInvoice
from impreza.exceptions import ResourceNotFound


def test_smoke_account_get_returns_balance(live_client: Client) -> None:
    """Sanity check: ``c.account.get()`` still works under the 1.7 changes."""
    me = live_client.account.get()
    assert isinstance(me.balance, float)
    assert me.currency
    print(
        f"\n  account id={me.id} balance={me.balance} {me.currency} "
        f"email={me.email}"
    )


def test_smoke_topup_status_decodes_existing_invoice(live_client: Client) -> None:
    """Decode a real top-up invoice into a typed :class:`TopupInvoice`.

    Set ``IMPREZA_TOPUP_INVOICE_ID`` to an existing top-up invoice ID
    on the test account to exercise this. Skips silently otherwise so
    the suite stays runnable without manual setup.
    """
    raw = os.environ.get("IMPREZA_TOPUP_INVOICE_ID")
    if not raw:
        pytest.skip(
            "IMPREZA_TOPUP_INVOICE_ID not set — set to an existing top-up "
            "invoice ID to exercise topup_status() decoding"
        )

    try:
        invoice_id = int(raw)
    except ValueError:
        pytest.fail(f"IMPREZA_TOPUP_INVOICE_ID must be an integer, got: {raw!r}")

    try:
        inv = live_client.account.topup_status(invoice_id)
    except ResourceNotFound:
        pytest.skip(
            f"top-up invoice {invoice_id} not found on this account — "
            "either it doesn't exist, isn't an AddFunds invoice, or "
            "belongs to a different client"
        )

    assert isinstance(inv, TopupInvoice)
    assert inv.invoice_id == invoice_id
    assert inv.status in {"pending", "paid", "cancelled", "refunded"}
    assert isinstance(inv.amount, float)
    assert inv.currency
    print(
        f"\n  topup invoice {inv.invoice_id}: status={inv.status!r}"
        f" amount={inv.amount} {inv.currency}"
        + (f" method={inv.method}" if inv.method else "")
        + (f" balance_after={inv.balance_after}" if inv.balance_after is not None else "")
    )


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="destructive: creates a real (unpaid) invoice; opt in via "
    "IMPREZA_DESTRUCTIVE_TESTS=1",
)
def test_smoke_destructive_topup_create_then_status(live_client: Client) -> None:
    """Full create-then-status cycle.

    Creates a real ``AddFunds`` invoice for $5.00 routed to btcpayinline
    (the configured AddFunds minimum). No payment is made — the
    invoice sits unpaid until it expires (2h) or is cancelled / marked
    paid. Verifies the create response decodes, the status
    endpoint returns ``pending`` immediately afterwards, and the payment
    URL points at the portal as expected.

    The invoice ID is printed prominently so a follow-on flow can mark it
    paid and validate the ``wait_until_paid`` polling against a
    real settlement.
    """
    inv = live_client.account.topup(amount=5.00, method="xmr")
    assert isinstance(inv, TopupInvoice)
    assert inv.invoice_id > 0
    assert inv.status == "pending"
    assert inv.payment_url is not None
    assert "viewinvoice.php" in inv.payment_url
    assert "id=" in inv.payment_url
    assert inv.expires_at is not None

    print(
        f"\n  created topup invoice {inv.invoice_id} for "
        f"{inv.amount} {inv.currency}; status={inv.status!r}; "
        f"expires_at={inv.expires_at}"
    )
    print(f"  payment_url: {inv.payment_url}")

    # Round-trip via topup_status — should match what we just got
    fresh = live_client.account.topup_status(inv.invoice_id)
    assert fresh.invoice_id == inv.invoice_id
    assert fresh.status == "pending"
    assert fresh.amount == inv.amount
    print(
        f"  topup_status({inv.invoice_id}) round-trip: "
        f"status={fresh.status!r} method={fresh.method}"
    )
