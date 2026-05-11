"""Live integration smoke tests for Phase 1.4b-ii (VPS backend-specific).

Read-only operations only. The mutating sub-resources (snapshots
create/rollback/delete, backups create/restore/delete, schedules,
rescue, iso, rdns set/delete, ssh_keys assign, vnc_password, resize,
boot_order, ipv6_enable, migrate, cancel, network_reconfigure) are
covered by the mocked unit suite. Driving them against a real VPS
requires opt-in destructive flags reserved for a future test suite.

The tests skip silently when the test account has no VPS services
(matching the 1.4b-i smoke pattern). When VPS services exist:

* For Proxmox VPS: list snapshots, list backups, list backup schedules,
  list operations queue.
* For Cloud VPS: list images.
* For both: confirm BackendNotSupported is raised on the wrong-backend
  property — a pure-SDK assertion that doesn't need a network call.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_4b_ii_smoke.py -v -s
"""

from __future__ import annotations

import pytest

from impreza import BackendNotSupported, Client


def _split_by_backend(client: Client) -> tuple[list, list]:  # type: ignore[type-arg]
    proxmox: list = []
    cloud: list = []
    for vps in client.vps.list():
        if vps.backend == "proxmox":
            proxmox.append(vps)
        else:
            cloud.append(vps)
    return proxmox, cloud


def test_smoke_proxmox_read_sub_resources(live_client: Client) -> None:
    """Read every Proxmox sub-resource on at least one Proxmox VPS, if available."""
    proxmox, _ = _split_by_backend(live_client)
    if not proxmox:
        pytest.skip("no Proxmox VPS on this account")

    vps = proxmox[0]
    snaps = vps.snapshots.list()
    backups = vps.backups.list()
    schedules = vps.backup_schedules.list()
    ops = vps.operations.list()
    print(
        f"\n  proxmox vps {vps.id}: "
        f"{len(snaps)} snapshots, {len(backups)} backups, "
        f"{len(schedules)} schedules, {len(ops)} operations"
    )


def test_smoke_cloud_images_list(live_client: Client) -> None:
    """List images on at least one Cloud VPS, if available."""
    _, cloud = _split_by_backend(live_client)
    if not cloud:
        pytest.skip("no Cloud VPS on this account")

    vps = cloud[0]
    images = vps.images.list()
    print(f"\n  cloud vps {vps.id}: {len(images)} saved image(s)")


def test_smoke_backend_mismatch_raises(live_client: Client) -> None:
    """BackendNotSupported is raised on wrong-backend access — even live.

    This assertion is mostly redundant with the unit suite, but running it
    against a real :class:`Vps` confirms the live path also constructs the
    bound model with a backend attribute and that the guard fires before
    any network call is made.
    """
    proxmox, cloud = _split_by_backend(live_client)
    if not proxmox and not cloud:
        pytest.skip("no VPS services on this account")

    if proxmox:
        with pytest.raises(BackendNotSupported):
            proxmox[0].images  # noqa: B018
    if cloud:
        with pytest.raises(BackendNotSupported):
            cloud[0].snapshots  # noqa: B018
