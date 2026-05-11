"""Live integration smoke for Phase 3.2 (VPS power verbs).

Power-cycling a real VPS is **real downtime** — every test in this
file is gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1`` so a casual
``pytest`` run can't accidentally take down a customer-facing host.

Required env::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    export IMPREZA_TEST_VPS_ID=12345         # a VPS you own and can safely power-cycle
    export IMPREZA_DESTRUCTIVE_TESTS=1       # confirms you understand the cost

Optional::

    export IMPREZA_TEST_VPS_POLL_SECONDS=20  # how long to wait for status to settle (default 20)
    export IMPREZA_TEST_VPS_POLL_INTERVAL=4  # poll cadence in seconds (default 4)

The smoke runs a full power cycle that nets to the original state:

    1. capture initial power state
    2. shutdown (graceful) → poll until ``stopped`` (skip if already stopped)
    3. start → poll until ``running``
    4. reboot → status check (no settle wait — Proxmox/Cloud can stay
       ``running`` throughout)

Force ``stop`` is exercised in a separate test so the corruption-
risk path gets coverage without bracketing every CI run with hard
power-offs.
"""

from __future__ import annotations

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
        pytest.skip("IMPREZA_TEST_VPS_ID not set — pick a VPS you can safely power-cycle")
    try:
        return int(raw)
    except ValueError:
        pytest.fail(f"IMPREZA_TEST_VPS_ID must be an integer, got: {raw!r}")


def _poll_seconds() -> int:
    return int(os.environ.get("IMPREZA_TEST_VPS_POLL_SECONDS", "20"))


def _poll_interval() -> int:
    return int(os.environ.get("IMPREZA_TEST_VPS_POLL_INTERVAL", "4"))


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


def _read_power_state(vps_id: int) -> str:
    """Pull the current power_state via ``vps status --output json``.
    Returns the literal string (``"running"``, ``"stopped"``,
    ``"online"``, ``"offline"`` depending on backend) so the caller
    can compare without forcing a normalisation."""
    import json

    result = runner.invoke(
        app, ["vps", "status", str(vps_id), "--output", "json"]
    )
    if result.exit_code != 0:
        pytest.fail(f"status failed: {result.stderr.strip()!r}")
    parsed = json.loads(result.stdout)
    state = parsed.get("power_state")
    if not isinstance(state, str):
        pytest.fail(f"unexpected status payload: {parsed!r}")
    return state


def _wait_until(vps_id: int, target_states: set[str]) -> str:
    """Poll ``power_state`` until it matches one of ``target_states``
    or the budget elapses. Returns whatever state we last observed
    so the caller can fail with a descriptive message."""
    budget = _poll_seconds()
    interval = _poll_interval()
    deadline = time.monotonic() + budget
    last = _read_power_state(vps_id)
    while last not in target_states and time.monotonic() < deadline:
        time.sleep(interval)
        last = _read_power_state(vps_id)
    return last


# Both backends report different idle / running tokens — accept all
# of the variants we've seen on the live API.
_STOPPED_STATES = {"stopped", "offline", "off"}
_RUNNING_STATES = {"running", "online"}


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="destructive: real power-cycle on a live VPS",
)
def test_smoke_power_cycle_round_trip(live_seeded_config: Path) -> None:
    """Graceful shutdown → start → reboot, netting back to running.

    Skips the shutdown step if the VPS is already stopped so the
    test still gets coverage for ``start`` + ``reboot`` even when
    re-run after a partial earlier execution.
    """
    vps_id = _test_vps_id()

    initial = _read_power_state(vps_id)
    print(f"\n  VPS {vps_id}: initial power_state={initial!r}")

    # ── Step 1: shutdown (skip if already stopped) ─────────────────
    if initial in _RUNNING_STATES:
        result = runner.invoke(app, ["vps", "shutdown", str(vps_id)])
        assert result.exit_code == 0, result.stderr
        assert "Graceful shutdown" in result.stdout
        print(f"    + shutdown sent: {result.stdout.strip()}")

        settled = _wait_until(vps_id, _STOPPED_STATES)
        assert settled in _STOPPED_STATES, (
            f"VPS {vps_id} did not reach a stopped state within "
            f"{_poll_seconds()}s — last observed {settled!r}"
        )
        print(f"    + settled to {settled!r}")
    else:
        print(f"    (skipped shutdown — already in {initial!r})")

    # ── Step 2: start ──────────────────────────────────────────────
    result = runner.invoke(app, ["vps", "start", str(vps_id)])
    assert result.exit_code == 0, result.stderr
    assert "Boot request sent" in result.stdout
    print(f"    + start sent: {result.stdout.strip()}")

    settled = _wait_until(vps_id, _RUNNING_STATES)
    assert settled in _RUNNING_STATES, (
        f"VPS {vps_id} did not reach a running state within "
        f"{_poll_seconds()}s — last observed {settled!r}"
    )
    print(f"    + settled to {settled!r}")

    # ── Step 3: reboot ─────────────────────────────────────────────
    # No settle wait — Proxmox/Cloud both stay reported as running
    # through a reboot. Just confirm the API accepted the request.
    result = runner.invoke(app, ["vps", "reboot", str(vps_id)])
    assert result.exit_code == 0, result.stderr
    assert "Reboot request sent" in result.stdout
    print(f"    + reboot sent: {result.stdout.strip()}")


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="destructive: force-stop may corrupt unwritten guest data",
)
def test_smoke_force_stop_with_yes(live_seeded_config: Path) -> None:
    """Hard power-off via ``vps stop --yes``. Brought back up at the
    end so the VPS is left in the same state we found it.

    Separated from the graceful round-trip on purpose — the force-
    stop is the most aggressive call in 3.2 and we want a single,
    explicit test that exercises it rather than including it in
    every smoke pass.
    """
    vps_id = _test_vps_id()

    initial = _read_power_state(vps_id)
    print(f"\n  VPS {vps_id}: initial power_state={initial!r}")

    # If already stopped, start first so we have something to force-stop.
    if initial in _STOPPED_STATES:
        boot = runner.invoke(app, ["vps", "start", str(vps_id)])
        assert boot.exit_code == 0, boot.stderr
        settled = _wait_until(vps_id, _RUNNING_STATES)
        assert settled in _RUNNING_STATES, (
            f"could not bring VPS {vps_id} up before force-stop test; "
            f"last state {settled!r}"
        )

    result = runner.invoke(app, ["vps", "stop", str(vps_id), "--yes"])
    assert result.exit_code == 0, result.stderr
    assert "Force-stop request sent" in result.stdout
    print(f"    + force-stop sent: {result.stdout.strip()}")

    settled = _wait_until(vps_id, _STOPPED_STATES)
    assert settled in _STOPPED_STATES, (
        f"VPS {vps_id} did not reach stopped after force-stop within "
        f"{_poll_seconds()}s — last observed {settled!r}"
    )
    print(f"    + settled to {settled!r}")

    # Restore: bring it back up so the smoke is net-neutral.
    boot = runner.invoke(app, ["vps", "start", str(vps_id)])
    assert boot.exit_code == 0, boot.stderr
    settled = _wait_until(vps_id, _RUNNING_STATES)
    print(f"    + restored to {settled!r}")
