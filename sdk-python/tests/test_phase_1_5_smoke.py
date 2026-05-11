"""Live integration smoke tests for Phase 1.5 (action polling).

The mutating operations that produce real :class:`Operation` futures
(snapshot rollback, backup create / restore, reinstall, migrate) are
gated behind ``IMPREZA_DESTRUCTIVE_TESTS=1`` because they do real work
on the upstream — even a successful snapshot create costs real
storage and a real Proxmox queue cycle.

What the *non-destructive* tests below verify is the **read** half of
the polling layer: the :class:`Operation` future can be constructed
from a real ``vps.operations.list()`` payload and its status predicates
work against live data.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_5_smoke.py -v -s
"""

from __future__ import annotations

import os

import pytest

from impreza import Client


def _first_proxmox_vps_id(client: Client) -> int | None:
    for vps in client.vps.list():
        if vps.backend == "proxmox":
            return vps.id
    return None


def test_smoke_operations_list_decodes_to_typed_models(live_client: Client) -> None:
    """``vps.operations.list()`` returns typed VpsOperation instances even
    when the queue is empty — the contract is just "list of objects with
    uuid and status if any exist". """
    sid = _first_proxmox_vps_id(live_client)
    if sid is None:
        pytest.skip("no Proxmox VPS on this account")

    ops = live_client.vps.get(sid).operations.list()
    assert isinstance(ops, list)
    for op in ops:
        assert isinstance(op.uuid, str) and op.uuid
        assert isinstance(op.status, str) and op.status
    print(f"\n  proxmox vps {sid}: {len(ops)} queued/running/recent op(s)")


def test_smoke_operations_get_round_trips_when_one_exists(
    live_client: Client,
) -> None:
    """If the queue has any operation, fetch its detail via ``operations.get``
    and confirm the shape decodes."""
    sid = _first_proxmox_vps_id(live_client)
    if sid is None:
        pytest.skip("no Proxmox VPS on this account")

    ops = live_client.vps.get(sid).operations.list()
    if not ops:
        pytest.skip(
            "operations queue is empty — nothing to fetch detail for "
            "(this is normal for healthy VPSes)"
        )

    detail = live_client.vps.get(sid).operations.get(ops[0].uuid)
    assert detail.uuid == ops[0].uuid
    assert detail.status
    print(
        f"\n  op {detail.uuid}: status={detail.status!r}"
        + (f", progress={detail.progress}" if detail.progress is not None else "")
    )


@pytest.mark.skipif(
    os.environ.get("IMPREZA_DESTRUCTIVE_TESTS") != "1",
    reason="destructive: creates a real Proxmox backup; opt in via "
    "IMPREZA_DESTRUCTIVE_TESTS=1",
)
def test_smoke_destructive_backup_create_then_wait(live_client: Client) -> None:
    """Create a real backup and wait for it to complete.

    Charges nothing extra (backups are part of the plan), but does take
    real disk and a real Proxmox queue cycle. Opt-in only.
    """
    sid = _first_proxmox_vps_id(live_client)
    if sid is None:
        pytest.skip("no Proxmox VPS on this account")

    op = live_client.vps.get(sid).backups.create()
    print(f"\n  queued backup op {op.uuid} (initial status={op.status!r})")

    # 30-minute ceiling — backups can take a while on busy clusters
    op.wait(timeout=1800.0, poll_interval=10.0)
    assert op.is_success()
    print(f"  backup op {op.uuid} completed")
