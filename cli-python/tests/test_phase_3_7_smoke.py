"""Live integration smoke for Phase 3.7 (Service cancel + webhooks).

Bucketed by reversibility, same pattern as prior 3.x:

* **Safe reads** (`webhook list`, `webhook event-types`,
  `webhook deliveries` on an existing subscription, if any):
  no state change. Gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1``
  for symmetry.
* **Reversible round-trip** (`webhook create + delete`): point
  to a sentinel URL the test owns (or just a clearly-marked
  fake URL) and clean up on the same invocation. Net-neutral.
  Gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1``.
* **Catastrophic** (`service cancel`): submits a real
  AddCancelRequest that staff would then have to roll back if
  the test fired it accidentally. Mock-only by design.

Required env::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    export IMPREZA_DESTRUCTIVE_TESTS=1
"""

from __future__ import annotations

import json
import os
import time
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


# ── webhook event-types: read the catalog ───────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account (gated for symmetry with other 3.x smokes)",
)
def test_smoke_webhook_event_types(live_seeded_config: Path) -> None:
    """Server-published catalog of subscribable event types."""
    result = runner.invoke(
        app, ["webhook", "event-types", "--output", "json"]
    )
    if result.exit_code != 0:
        pytest.fail(f"event-types failed: {result.stderr.strip()!r}")
    parsed = json.loads(result.stdout)
    # At minimum the server should expose *some* event types.
    assert isinstance(parsed.get("event_types"), list)
    print(
        f"\n  Event-type catalog: {len(parsed['event_types'])} concrete, "
        f"{len(parsed.get('wildcards', {}))} wildcard(s)"
    )


# ── webhook list: read existing subscriptions ───────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="reads from a real account",
)
def test_smoke_webhook_list(live_seeded_config: Path) -> None:
    """Empty or populated, both legitimate."""
    result = runner.invoke(app, ["webhook", "list", "--output", "json"])
    if result.exit_code != 0:
        pytest.fail(f"webhook list failed: {result.stderr.strip()!r}")
    if result.stdout.strip().startswith("["):
        subs = json.loads(result.stdout)
        print(f"\n  Account has {len(subs)} webhook subscription(s)")
    else:
        print("\n  Account has no webhook subscriptions")


# ── webhook create + delete round-trip ──────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="creates and immediately deletes a webhook subscription on a real account",
)
def test_smoke_webhook_create_delete_round_trip(
    live_seeded_config: Path,
) -> None:
    """Create a sentinel subscription pointing to a non-routable
    test URL, verify via list, then delete. The receiver URL never
    actually receives anything — the server may attempt one
    delivery and log the connection failure, which is the expected
    behaviour for a non-routable target.

    Net-neutral by design: the sentinel URL is unique per run
    (timestamp suffix), and the delete step always runs even if
    intermediate assertions fail.
    """
    sentinel_url = (
        f"https://phase37-smoke-{int(time.time())}.example.test/hook"
    )
    print(f"\n  Sentinel URL: {sentinel_url}")

    # Step 1 — create.
    create_result = runner.invoke(
        app,
        [
            "webhook", "create",
            "--url", sentinel_url,
            "--event", "topup.paid",
            "--description", "phase-3.7 smoke; auto-deleted",
        ],
    )
    if create_result.exit_code != 0:
        pytest.fail(f"create failed: {create_result.stderr.strip()!r}")
    # Parse the new subscription id out of the human-readable
    # output ("Subscription <id> created: ...").
    first_line = create_result.stdout.splitlines()[0]
    # Expected shape: "Subscription 42 created: https://..."
    parts = first_line.split()
    assert len(parts) >= 2 and parts[0] == "Subscription"
    new_id = int(parts[1])
    print(f"    + create OK: subscription {new_id}")

    # Step 2 — confirm via list (best-effort). The order of
    # subscriptions on the list endpoint isn't documented; just
    # check that ours is in there.
    delete_failure: str | None = None
    try:
        list_result = runner.invoke(
            app, ["webhook", "list", "--output", "json"]
        )
        if list_result.exit_code == 0 and list_result.stdout.strip().startswith("["):
            subs = json.loads(list_result.stdout)
            matched = [s for s in subs if s.get("id") == new_id]
            if matched:
                print(
                    f"    + list confirms sentinel present "
                    f"({len(subs)} total subscriptions)"
                )
            else:
                print(
                    f"    ! sentinel not in list ({len(subs)} subscriptions); "
                    "continuing to delete anyway"
                )
    finally:
        # Step 3 — delete (cleanup). Always run; pytest.fail
        # below if this itself fails so the leftover doesn't
        # quietly accumulate.
        del_result = runner.invoke(
            app,
            ["webhook", "delete", str(new_id), "--yes"],
        )
        if del_result.exit_code != 0:
            delete_failure = del_result.stderr.strip()
        else:
            print(f"    + delete OK: subscription {new_id}")

    if delete_failure:
        pytest.fail(
            f"delete failed: {delete_failure!r}. Sentinel subscription "
            f"{new_id} may still exist on the account."
        )
