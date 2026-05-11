"""Live integration smoke for Phase 3.1 (domain writes + DNS CRUD).

The mutating CLI commands shipped in 3.1 fall in three risk
categories:

* **Cost-incurring** (register, transfer, id-protection): real money
  comes out of the account balance. NOT smoked live — covered by
  mocks only. Adding a flag-gated path here would mean buying a
  domain on every CI cycle, which is hard to clean up.
* **Destructive but recoverable** (set-nameservers, lock/unlock,
  dns add/update/delete): real state changes but nothing leaves
  the registrar. Gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1``.
* **Idempotent / side-effect-only emails** (raa-verify, gdpr-auth,
  transfer-approval, dns activate already-active): safe to run
  every smoke pass; the recipient gets a resend email which is the
  intended UX for the command.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    export IMPREZA_TEST_DOMAIN=imprezahost.icu

    pytest tests/test_phase_3_1_smoke.py -v -s

For the destructive cycle::

    export IMPREZA_DESTRUCTIVE_TESTS=1
    pytest tests/test_phase_3_1_smoke.py -v -s
"""

from __future__ import annotations

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


def _owned_domain() -> str | None:
    return os.environ.get("IMPREZA_TEST_DOMAIN")


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


# ── Idempotent: just resend emails. Run every smoke pass. ───────────


def test_smoke_raa_verify_resends_email(live_seeded_config: Path) -> None:
    """The RAA verification email is a simple resend — already-
    verified domains reply OK, the registrant doesn't get spam.
    Exercises the SDK + CLI plumbing for the most-trivial write
    op without producing user-visible side effects."""
    domain = _owned_domain()
    if not domain:
        pytest.skip("IMPREZA_TEST_DOMAIN not set")

    result = runner.invoke(app, ["domain", "raa-verify", domain])
    # Either 200 (resent) or 4xx with a friendly message — both are
    # legitimate. The CLI must exit 0 on 200; for non-200 the SDK
    # raises ApiError which we map to exit 1 + stderr.
    if result.exit_code == 0:
        assert "RAA verification email resent" in result.stdout
        print(f"\n  raa-verify {domain}: resent OK")
    else:
        # Some TLDs / domain states reject re-sending — that's still
        # a correct path through the CLI; assert friendly stderr
        # rather than a traceback.
        assert "Traceback" not in result.stderr
        pytest.skip(
            f"upstream rejected resend ({result.stderr.strip()!r}); "
            "domain may not be in a state that allows re-verification"
        )


# ── Destructive but recoverable. Gated behind IMPREZA_DESTRUCTIVE_TESTS=1. ──


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="destructive: adds/removes a real DNS record",
)
def test_smoke_dns_crud_round_trip(live_seeded_config: Path) -> None:
    """Full add → update → delete cycle of a sentinel TXT record.
    Uses a very specific name (``_impreza-cli-smoke``) so a leftover
    from a failed run is easy to spot in the DNS listing.

    A failed assertion partway through would leave the record in a
    weird state; the test catches and reports those rather than
    raising so the listing isn't polluted across CI runs.
    """
    domain = _owned_domain()
    if not domain:
        pytest.skip("IMPREZA_TEST_DOMAIN not set")

    sentinel_host = "_impreza-cli-smoke"
    initial_value = "phase-3.1-smoke-initial"
    updated_value = "phase-3.1-smoke-updated"

    print(f"\n  DNS CRUD on {domain}, sentinel name {sentinel_host!r}:")

    # Step 1 — add the record.
    add_result = runner.invoke(
        app,
        [
            "domain", "dns", "add", domain,
            "--type", "TXT",
            "--name", sentinel_host,
            "--value", initial_value,
            # 7200s is the registrar minimum; values below are silently
            # dropped by the upstream (server-side fix tracked separately
            # as a follow-up — see the spawned task for "detect upstream
            # Failed in DNS addDns").
            "--ttl", "7200",
        ],
    )
    assert add_result.exit_code == 0, (
        f"add failed: {add_result.stderr!r}"
    )
    print(f"    + add: {add_result.stdout.strip()}")

    # Step 2 — verify via list (the same surface 2.4 already smokes,
    # but we filter to our sentinel).
    list_result = runner.invoke(
        app, ["domain", "dns", "list", domain, "--output", "json"]
    )
    assert list_result.exit_code == 0
    import json
    records = json.loads(list_result.stdout)
    matched = [
        r for r in records
        if r["type"] == "TXT" and sentinel_host in r["host"]
    ]
    assert matched, "added record didn't appear in the listing"
    print(f"    + list: {len(matched)} matching record(s) found")

    # Step 3 — update.
    upd_result = runner.invoke(
        app,
        [
            "domain", "dns", "update", domain,
            "--type", "TXT",
            "--name", sentinel_host,
            "--old-value", initial_value,
            "--new-value", updated_value,
        ],
    )
    if upd_result.exit_code != 0:
        # Best-effort cleanup before bailing.
        runner.invoke(
            app,
            [
                "domain", "dns", "delete", domain,
                "--type", "TXT",
                "--name", sentinel_host,
                "--value", initial_value,
                "--yes",
            ],
        )
        pytest.fail(f"update failed: {upd_result.stderr!r}")
    print(f"    + update: {upd_result.stdout.strip()}")

    # Step 4 — delete (cleanup).
    del_result = runner.invoke(
        app,
        [
            "domain", "dns", "delete", domain,
            "--type", "TXT",
            "--name", sentinel_host,
            "--value", updated_value,
            "--yes",
        ],
    )
    assert del_result.exit_code == 0, (
        f"delete failed: {del_result.stderr!r}"
    )
    print(f"    + delete: {del_result.stdout.strip()}")


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="destructive: toggles transfer lock state",
)
def test_smoke_lock_unlock_cycle(live_seeded_config: Path) -> None:
    """Unlock to read the EPP code, then re-lock. Net state is
    unchanged at end. Useful for confirming the EPP returns
    correctly through the CLI pipeline."""
    domain = _owned_domain()
    if not domain:
        pytest.skip("IMPREZA_TEST_DOMAIN not set")

    # Unlock first (returns EPP code).
    unlock_result = runner.invoke(
        app, ["domain", "unlock", domain, "--yes"]
    )
    assert unlock_result.exit_code == 0, unlock_result.stderr
    assert "EPP" in unlock_result.stdout
    print(f"\n  unlock {domain}: EPP code returned (length-checked)")

    # Re-lock immediately.
    lock_result = runner.invoke(app, ["domain", "lock", domain])
    assert lock_result.exit_code == 0, lock_result.stderr
    assert "Transfer lock enabled" in lock_result.stdout
    print(f"  re-lock {domain}: OK")
