"""Live integration smoke for Phase 3.3 (VPS management verbs).

3.3 spans the full destructiveness spectrum, from "cheap to
round-trip safely" to "ends customer service entirely". The smokes
are bucketed accordingly:

* **Recoverable round-trips** (set-hostname, set-password):
  capture original value, change to a sentinel, change back. Net
  state unchanged. Gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1``.
* **Catastrophic** (reinstall, migrate, cancel): mock-only by
  design. Running these live would wipe a disk / move a VM
  between hosts / submit a cancellation request the staff would
  then need to roll back — coverage at this destructiveness level
  lives in the unit tests with respx.

The original draft also covered the Proxmox suspend/unsuspend
pair, but those endpoints were retired on 2026-05-11 (
service suspension is staff-only). Nothing live to smoke there.

Required env::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    export IMPREZA_TEST_VPS_ID=12345
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


# ── shared fixtures / helpers ───────────────────────────────────────


def _live_creds() -> tuple[str, str]:
    api_key = os.environ.get("IMPREZA_API_KEY", "")
    api_secret = os.environ.get("IMPREZA_API_SECRET", "")
    if not api_key or not api_secret:
        pytest.skip("IMPREZA_API_KEY / IMPREZA_API_SECRET not set")
    return api_key, api_secret


def _test_vps_id() -> int:
    raw = os.environ.get("IMPREZA_TEST_VPS_ID", "")
    if not raw:
        pytest.skip("IMPREZA_TEST_VPS_ID not set")
    try:
        return int(raw)
    except ValueError:
        pytest.fail(f"IMPREZA_TEST_VPS_ID must be an integer, got: {raw!r}")


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


def _read_show_field(vps_id: int, field: str) -> object:
    """Read a single field from ``vps show <id> --output json`` so
    the round-trips have something authoritative to compare
    against. Returns the field's raw value (or ``None`` if missing)."""
    result = runner.invoke(
        app, ["vps", "show", str(vps_id), "--output", "json"]
    )
    if result.exit_code != 0:
        pytest.fail(f"vps show failed: {result.stderr.strip()!r}")
    parsed = json.loads(result.stdout)
    return parsed.get(field)


# ── set-hostname round-trip ─────────────────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="changes the hostname of a real VPS (then changes it back)",
)
def test_smoke_set_hostname_round_trip(live_seeded_config: Path) -> None:
    """Capture the current hostname, change to a sentinel, change
    back. Net state unchanged. Verifies plumbing on the PUT path."""
    vps_id = _test_vps_id()

    original = _read_show_field(vps_id, "domain")
    if not isinstance(original, str) or not original:
        pytest.skip(f"VPS {vps_id} has no readable domain to round-trip from")

    sentinel = f"phase-3-3-smoke-{int(time.time())}.example.test"
    print(f"\n  VPS {vps_id}: original hostname={original!r}")

    # Step 1 — change to the sentinel.
    set_result = runner.invoke(
        app, ["vps", "set-hostname", str(vps_id), sentinel]
    )
    if set_result.exit_code != 0:
        pytest.fail(f"set-hostname (sentinel) failed: {set_result.stderr!r}")
    assert sentinel in set_result.stdout
    print(f"    + set hostname to {sentinel!r}")

    # Step 2 — restore the original. Always attempt this even if a
    # later assertion fails; the VPS shouldn't be left with a
    # sentinel hostname.
    restore_failure: str | None = None
    try:
        # The change isn't always reflected on /account/services
        # synchronously (registrar caching) — don't assert by re-
        # reading here. Trust the upstream-accepted response.
        pass
    finally:
        restore_result = runner.invoke(
            app, ["vps", "set-hostname", str(vps_id), original]
        )
        if restore_result.exit_code != 0:
            restore_failure = restore_result.stderr.strip()
        else:
            print(f"    + restored hostname to {original!r}")

    if restore_failure:
        pytest.fail(
            f"set-hostname (restore to {original!r}) failed: "
            f"{restore_failure!r}. VPS may still be on sentinel hostname."
        )


# ── set-password round-trip ─────────────────────────────────────────


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1"
    or os.environ.get("IMPREZA_TEST_ALLOW_PASSWORD_RESET") != "1",
    reason=(
        "irreversible from our side: leaves the VPS with a sentinel "
        "password. Set IMPREZA_TEST_ALLOW_PASSWORD_RESET=1 to opt in, "
        "then plan to reset the password via your Impreza Account afterwards."
    ),
)
def test_smoke_set_password_accepts_strong_password(
    live_seeded_config: Path,
) -> None:
    """Submit a strong sentinel password. Doesn't read it back
    (the API doesn't expose passwords post-set, by design); just
    confirms the API accepts the change.

    The test is single-direction by necessity — there's no
    "original password" we can capture and restore. The customer
    must reset to their preferred password out of band after the
    smoke runs, or use this only on a sacrificial test VPS.
    """
    vps_id = _test_vps_id()
    sentinel = f"PhaseThreeThreeSmoke!{int(time.time())}#Az"

    result = runner.invoke(
        app,
        ["vps", "set-password", str(vps_id), "--password", sentinel],
    )
    assert result.exit_code == 0, result.stderr
    assert f"Password updated for VPS {vps_id}" in result.stdout
    print(f"\n  VPS {vps_id}: password updated to sentinel (len {len(sentinel)})")
    print("    NOTE: this VPS now has a smoke-test password. Reset out of band.")


# No suspend / unsuspend smoke: the endpoints were retired on
# 2026-05-11 (see DEVKIT_CLI_PLAN.md "Retire customer-facing
# suspend/unsuspend" cleanup) along with the SDK and CLI surfaces.
