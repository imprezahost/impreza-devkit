"""Live integration smoke for Phase 3.6 (Orders + crypto top-up).

Bucketed by reversibility, same pattern as 3.3 / 3.4 / 3.5:

* **Safe read-only** (`order list`, `order show`): no state change.
  Gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1`` for symmetry with
  other 3.x smokes.
* **Cost-incurring** (`order create`, `order upgrade`, `account
  topup`): all charge real money — `create` and `upgrade` debit
  the account balance immediately for the prorated cost, and
  `topup` creates an actual BTCPay invoice the customer would
  open in a browser. Mock-only by design (matches 3.1's policy
  for `register` / `transfer` / `id-protection`).

If the test account has zero historical orders, the read smokes
skip cleanly — that's a legitimate fresh-account state, not a
failure.

Required env::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    export IMPREZA_DESTRUCTIVE_TESTS=1
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest
from typer.testing import CliRunner

from impreza_cli.config import Config
from impreza_cli.main import app

runner = CliRunner()


def _live_creds() -> tuple[str, str]:
    api_key = os.environ.get("IMPREZA_API_KEY", "")
    api_secret = os.environ.get("IMPREZA_API_SECRET", "")
    if not api_key or not api_secret:
        pytest.skip("IMPREZA_API_KEY / IMPREZA_API_SECRET not set")
    return api_key, api_secret


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


# ── order list ──────────────────────────────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account (gated for symmetry with other 3.x smokes)",
)
def test_smoke_order_list(live_seeded_config: Path) -> None:
    """Read-only — succeeds whether the account has historical
    orders or not. Empty account is a legitimate fresh state."""
    result = runner.invoke(app, ["order", "list", "--output", "json"])
    if result.exit_code != 0:
        pytest.fail(f"order list failed: {result.stderr.strip()!r}")
    if result.stdout.strip().startswith("["):
        orders = json.loads(result.stdout)
        print(f"\n  Account has {len(orders)} historical order(s)")
    else:
        print("\n  Account has no historical orders")


# ── order show (only if there's at least one order to show) ─────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account",
)
def test_smoke_order_show_first(live_seeded_config: Path) -> None:
    """Resolve the most recent order id from `order list`, then
    fetch its detail. Skips cleanly on a fresh account with no
    orders."""
    list_result = runner.invoke(
        app, ["order", "list", "--output", "json"]
    )
    if list_result.exit_code != 0:
        pytest.fail(f"order list failed: {list_result.stderr.strip()!r}")
    if not list_result.stdout.strip().startswith("["):
        pytest.skip("account has no historical orders to show")
    orders = json.loads(list_result.stdout)
    if not orders:
        pytest.skip("account has no historical orders to show")

    first_id = orders[0]["id"]
    show_result = runner.invoke(
        app, ["order", "show", str(first_id), "--output", "json"]
    )
    if show_result.exit_code != 0:
        pytest.fail(f"order show {first_id} failed: {show_result.stderr.strip()!r}")
    detail = json.loads(show_result.stdout)
    assert detail["id"] == first_id
    items_count = len(detail.get("items") or [])
    print(
        f"\n  Order {first_id}: status={detail.get('status')!r}, "
        f"{items_count} line item(s)"
    )
