"""Unit tests for backend-specific VPS sub-resources (Phase 1.4b-ii).

Covers Proxmox-only sub-resources (snapshots, backups, backup_schedules,
operations) and inline methods (info, config, pending, resources, ips,
available_ips, templates, locations, console, console_ssh,
network_reconfigure, migrate); Cloud-only sub-resources (images,
rescue, iso, rdns, ssh_keys) and inline methods (vnc, vnc_password,
resize, boot_order, ipv6_enable); the shared ``cancel`` operation;
and the ``BackendNotSupported`` mismatch guards.

The ``suspend`` / ``unsuspend`` pair was retired on 2026-05-11 (see
the same-named comment in :mod:`impreza.resources.vps`) — no tests
remain for those.

Mocked via ``respx`` — no real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import (
    AsyncClient,
    BackendNotSupported,
    Backup,
    BackupSchedule,
    Client,
    ConsoleUrl,
    Image,
    Snapshot,
    SshConsole,
    SshKey,
    VncCredentials,
    VpsOperation,
)

BASE = "https://api.imprezahost.com/v1"


# ── payload helpers ────────────────────────────────────────────────────


def _service_payload(
    service_id: int,
    *,
    backend: str = "proxmox",
) -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": service_id,
            "domain": "vps.example.com",
            "status": "Active",
            "product": "VPS Plan 2",
            "product_group": "VPS Hosting",
            "billing_cycle": "monthly",
            "amount": 15.0,
            "dedicated_ip": "185.100.86.42",
            "registered_at": "2024-06-01",
            "next_due": "2026-04-01",
            "vps_backend": backend,
        },
        "meta": {"request_id": "req_test"},
    }


def _ok(data: dict[str, object] | list[object] | None = None) -> dict[str, object]:
    return {
        "success": True,
        "data": data if data is not None else {"message": "ok"},
        "meta": {"request_id": "req_test"},
    }


def _wrap_list(key: str, items: list[dict[str, object]]) -> dict[str, object]:
    return _ok({key: items})


def _setup_proxmox_vps(service_id: int = 100) -> None:
    respx.get(f"{BASE}/account/services/{service_id}").mock(
        return_value=httpx.Response(200, json=_service_payload(service_id, backend="proxmox"))
    )


def _setup_cloud_vps(service_id: int = 100) -> None:
    respx.get(f"{BASE}/account/services/{service_id}").mock(
        return_value=httpx.Response(200, json=_service_payload(service_id, backend="cloud"))
    )


# ── BackendNotSupported guards ─────────────────────────────────────────


@respx.mock
def test_snapshots_on_cloud_raises_backend_not_supported() -> None:
    _setup_cloud_vps(1)
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(1)
        with pytest.raises(BackendNotSupported) as exc_info:
            vps.snapshots  # noqa: B018  — accessing the property is the trigger
    err = exc_info.value
    assert err.backend == "cloud"
    assert err.operation == "snapshots"


@respx.mock
def test_images_on_proxmox_raises_backend_not_supported() -> None:
    _setup_proxmox_vps(2)
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(2)
        with pytest.raises(BackendNotSupported) as exc_info:
            vps.images  # noqa: B018
    assert exc_info.value.backend == "proxmox"
    assert exc_info.value.operation == "images"


@respx.mock
def test_inline_proxmox_op_on_cloud_raises() -> None:
    _setup_cloud_vps(3)
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(3)
        with pytest.raises(BackendNotSupported):
            vps.config()


@respx.mock
def test_inline_cloud_op_on_proxmox_raises() -> None:
    _setup_proxmox_vps(4)
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(4)
        with pytest.raises(BackendNotSupported):
            vps.vnc()


# ── Proxmox snapshots ──────────────────────────────────────────────────


@respx.mock
def test_proxmox_snapshots_list() -> None:
    _setup_proxmox_vps(10)
    respx.get(f"{BASE}/vps/proxmox/10/snapshots").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_list(
                "snapshots",
                [
                    {
                        "name": "pre-update",
                        "description": "before apt upgrade",
                        "created_at": "2026-05-01",
                    },
                    {"name": "snap-2", "description": None, "created_at": "2026-05-04"},
                ],
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        snaps = c.vps.get(10).snapshots.list()
    assert len(snaps) == 2
    assert all(isinstance(s, Snapshot) for s in snaps)
    assert snaps[0].name == "pre-update"


@respx.mock
def test_proxmox_snapshots_create_with_description() -> None:
    _setup_proxmox_vps(11)
    route = respx.post(f"{BASE}/vps/proxmox/11/snapshots").mock(
        return_value=httpx.Response(200, json=_ok({"name": "new-snap", "description": "d"}))
    )
    with Client(api_key="x", api_secret="y") as c:
        snap = c.vps.get(11).snapshots.create("new-snap", description="d")
    assert isinstance(snap, Snapshot)
    body = route.calls.last.request.read()
    assert b"new-snap" in body
    assert b'"description"' in body


@respx.mock
def test_proxmox_snapshots_delete_and_rollback() -> None:
    """rollback() now returns an Operation future (Phase 1.5)."""
    _setup_proxmox_vps(12)
    delete = respx.delete(f"{BASE}/vps/proxmox/12/snapshots/snap-1").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    rollback = respx.post(f"{BASE}/vps/proxmox/12/snapshots/snap-1/rollback").mock(
        return_value=httpx.Response(
            200, json=_ok({"uuid": "rb-001", "status": "queued"})
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(12)
        vps.snapshots.delete("snap-1")
        op = vps.snapshots.rollback("snap-1")
    assert delete.called and rollback.called
    # Operation future returned with the queued state — caller can wait()
    from impreza import Operation as _Operation
    assert isinstance(op, _Operation)
    assert op.uuid == "rb-001"
    assert op.status == "queued"


# ── Proxmox backups ────────────────────────────────────────────────────


@respx.mock
def test_proxmox_backups_list_and_lifecycle() -> None:
    """create() + restore() now return Operation futures (Phase 1.5)."""
    _setup_proxmox_vps(20)
    respx.get(f"{BASE}/vps/proxmox/20/backups").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_list(
                "backups",
                [{"id": 1, "date": "2026-05-01", "size": 1234, "mode": "snapshot"}],
            ),
        )
    )
    create = respx.post(f"{BASE}/vps/proxmox/20/backups").mock(
        return_value=httpx.Response(
            200, json=_ok({"uuid": "bk-create-001", "status": "queued"})
        )
    )
    restore = respx.post(f"{BASE}/vps/proxmox/20/backups/1/restore").mock(
        return_value=httpx.Response(
            200, json=_ok({"uuid": "bk-restore-001", "status": "running", "progress": 25})
        )
    )
    delete = respx.delete(f"{BASE}/vps/proxmox/20/backups/1").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    from impreza import Operation as _Operation

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(20)
        backups = vps.backups.list()
        assert len(backups) == 1 and isinstance(backups[0], Backup)

        op_create = vps.backups.create()
        assert isinstance(op_create, _Operation)
        assert op_create.uuid == "bk-create-001"

        op_restore = vps.backups.restore(1)
        assert isinstance(op_restore, _Operation)
        assert op_restore.uuid == "bk-restore-001"
        assert op_restore.progress == 25

        vps.backups.delete(1)
    assert create.called and restore.called and delete.called


# ── Proxmox backup schedules ───────────────────────────────────────────


@respx.mock
def test_proxmox_schedules_create_with_options() -> None:
    _setup_proxmox_vps(30)
    route = respx.post(f"{BASE}/vps/proxmox/30/backup-schedules").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"id": 1, "dow": "mon,wed,fri", "hour": 3, "minute": 30, "mode": "snapshot"}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        sched = c.vps.get(30).backup_schedules.create(
            dow="mon,wed,fri", hour=3, minute=30, mode="snapshot", compress="zstd"
        )
    assert isinstance(sched, BackupSchedule)
    body = route.calls.last.request.read()
    assert b"mon,wed,fri" in body
    assert b'"mode"' in body and b"snapshot" in body
    assert b'"compress"' in body and b"zstd" in body


@respx.mock
def test_proxmox_schedules_delete() -> None:
    _setup_proxmox_vps(31)
    route = respx.delete(f"{BASE}/vps/proxmox/31/backup-schedules/7").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        c.vps.get(31).backup_schedules.delete(7)
    assert route.called


# ── Proxmox operations ─────────────────────────────────────────────────


@respx.mock
def test_proxmox_operations_list_and_get() -> None:
    _setup_proxmox_vps(40)
    respx.get(f"{BASE}/vps/proxmox/40/operations").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_list(
                "operations",
                [{"uuid": "u1", "status": "running", "progress": 42.5}],
            ),
        )
    )
    respx.get(f"{BASE}/vps/proxmox/40/operations/u1").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"uuid": "u1", "status": "completed", "progress": 100}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(40)
        ops = vps.operations.list()
        assert len(ops) == 1 and isinstance(ops[0], VpsOperation)
        op = vps.operations.get("u1")
        assert op.status == "completed"


# ── Proxmox inline methods ─────────────────────────────────────────────


@respx.mock
def test_proxmox_info_methods_round_trip() -> None:
    _setup_proxmox_vps(50)
    def _stub(path: str, body: dict[str, object]) -> None:
        respx.get(f"{BASE}/vps/proxmox/50{path}").mock(
            return_value=httpx.Response(200, json=_ok(body)),
        )

    _stub("",                {"hostname": "h"})
    _stub("/config",         {"cores": 2})
    _stub("/pending",        {})
    _stub("/resources",      {"cpu": 12.5})
    _stub("/ips",            {"ips": []})
    _stub("/available-ips",  {"v4": 5})
    _stub("/templates",      {"templates": []})
    _stub("/locations",      {"locations": []})
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(50)
        assert vps.info() == {"hostname": "h"}
        assert vps.config() == {"cores": 2}
        assert vps.pending() == {}
        assert vps.resources() == {"cpu": 12.5}
        assert vps.ips() == {"ips": []}
        assert vps.available_ips() == {"v4": 5}
        assert vps.templates() == {"templates": []}
        assert vps.locations() == {"locations": []}


@respx.mock
def test_proxmox_console_returns_console_url() -> None:
    _setup_proxmox_vps(51)
    respx.get(f"{BASE}/vps/proxmox/51/console").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "url": "https://novnc.example.com/?token=abc",
                "expires_at": "2026-05-08T18:00:00Z",
            }),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        url = c.vps.get(51).console()
    assert isinstance(url, ConsoleUrl)
    assert url.url.startswith("https://novnc")


@respx.mock
def test_proxmox_console_ssh_posts_password() -> None:
    _setup_proxmox_vps(52)
    route = respx.post(f"{BASE}/vps/proxmox/52/console/ssh").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"ws_url": "wss://...", "encrypted_token": "tok", "expires_at": None}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        ssh = c.vps.get(52).console_ssh(password="root")
    assert isinstance(ssh, SshConsole)
    assert b"root" in route.calls.last.request.read()


@respx.mock
def test_proxmox_migrate_and_network() -> None:
    """migrate() returns an Operation future (Phase 1.5); network
    reconfigure stays as a dict."""
    _setup_proxmox_vps(53)
    migrate = respx.post(f"{BASE}/vps/proxmox/53/migrate").mock(
        return_value=httpx.Response(
            200, json=_ok({"uuid": "mg-001", "status": "queued"})
        )
    )
    netreconf = respx.post(f"{BASE}/vps/proxmox/53/network/reconfigure").mock(
        return_value=httpx.Response(200, json=_ok({"applied": True}))
    )
    from impreza import Operation as _Operation

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(53)
        op = vps.migrate(target="dc-2")
        assert isinstance(op, _Operation)
        assert op.uuid == "mg-001"
        assert vps.network_reconfigure() == {"applied": True}
    assert migrate.called and netreconf.called
    assert b"dc-2" in migrate.calls.last.request.read()


# ── Cloud images ───────────────────────────────────────────────────────


@respx.mock
def test_cloud_images_list_uses_account_level_path() -> None:
    """list() hits /vps/cloud/images (account-scoped); create() is per-VM."""
    _setup_cloud_vps(60)
    list_route = respx.get(f"{BASE}/vps/cloud/images").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_list("images", [{"id": 1, "name": "img1", "vm_id": 60}]),
        )
    )
    create_route = respx.post(f"{BASE}/vps/cloud/60/images").mock(
        return_value=httpx.Response(200, json=_ok({"id": 2, "name": "img2", "vm_id": 60}))
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(60)
        images = vps.images.list()
        assert len(images) == 1 and isinstance(images[0], Image)
        new_image = vps.images.create()
        assert new_image.id == 2
    assert list_route.called and create_route.called


@respx.mock
def test_cloud_images_restore_and_delete_use_account_path() -> None:
    _setup_cloud_vps(61)
    restore = respx.post(f"{BASE}/vps/cloud/61/images/5/restore").mock(
        return_value=httpx.Response(200, json=_ok({"queued": True}))
    )
    # Delete is account-scoped: /vps/cloud/images/{id} (no vmId in the path)
    delete = respx.delete(f"{BASE}/vps/cloud/images/5").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(61)
        vps.images.restore(5)
        vps.images.delete(5)
    assert restore.called and delete.called


# ── Cloud rescue / iso ─────────────────────────────────────────────────


@respx.mock
def test_cloud_rescue_enable_disable() -> None:
    _setup_cloud_vps(70)
    enable = respx.post(f"{BASE}/vps/cloud/70/rescue").mock(
        return_value=httpx.Response(200, json=_ok({"enabled": True}))
    )
    disable = respx.delete(f"{BASE}/vps/cloud/70/rescue").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(70)
        assert vps.rescue.enable(password="rescue!") == {"enabled": True}
        vps.rescue.disable()
    assert enable.called and disable.called
    assert b"rescue!" in enable.calls.last.request.read()


@respx.mock
def test_cloud_iso_mount_unmount() -> None:
    _setup_cloud_vps(71)
    mount = respx.post(f"{BASE}/vps/cloud/71/iso/mount").mock(
        return_value=httpx.Response(200, json=_ok({"mounted": "ubuntu.iso"}))
    )
    unmount = respx.delete(f"{BASE}/vps/cloud/71/iso").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(71)
        assert vps.iso.mount("ubuntu.iso") == {"mounted": "ubuntu.iso"}
        vps.iso.unmount()
    assert mount.called and unmount.called


# ── Cloud rdns ─────────────────────────────────────────────────────────


@respx.mock
def test_cloud_rdns_get_set_delete() -> None:
    _setup_cloud_vps(80)
    get = respx.get(f"{BASE}/vps/cloud/rdns/185.100.86.42").mock(
        return_value=httpx.Response(200, json=_ok({"hostname": "old.example.com"}))
    )
    put = respx.put(f"{BASE}/vps/cloud/rdns/185.100.86.42").mock(
        return_value=httpx.Response(200, json=_ok({"hostname": "new.example.com"}))
    )
    delete = respx.delete(f"{BASE}/vps/cloud/rdns/185.100.86.42").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(80)
        assert vps.rdns.get("185.100.86.42") == {"hostname": "old.example.com"}
        assert vps.rdns.set("185.100.86.42", "new.example.com") == {"hostname": "new.example.com"}
        vps.rdns.delete("185.100.86.42")
    assert get.called and put.called and delete.called
    assert b"new.example.com" in put.calls.last.request.read()


# ── Cloud ssh keys ─────────────────────────────────────────────────────


@respx.mock
def test_cloud_ssh_keys_list_and_assign() -> None:
    _setup_cloud_vps(90)
    list_route = respx.get(f"{BASE}/vps/cloud/ssh-keys").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_list("keys", [{"id": 1, "name": "laptop"}, {"id": 2, "name": "ci"}]),
        )
    )
    assign = respx.post(f"{BASE}/vps/cloud/90/ssh-keys").mock(
        return_value=httpx.Response(200, json=_ok({"assigned": [1, 2]}))
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(90)
        keys = vps.ssh_keys.list()
        assert len(keys) == 2 and isinstance(keys[0], SshKey)
        result = vps.ssh_keys.assign([1, 2])
        assert result == {"assigned": [1, 2]}
    assert list_route.called and assign.called


# ── Cloud inline methods ───────────────────────────────────────────────


@respx.mock
def test_cloud_vnc_returns_credentials() -> None:
    _setup_cloud_vps(100)
    respx.get(f"{BASE}/vps/cloud/100/vnc").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"ip": "vnc.example.com", "port": 5901, "password": "vnc-secret"}),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        creds = c.vps.get(100).vnc()
    assert isinstance(creds, VncCredentials)
    assert creds.port == 5901


@respx.mock
def test_cloud_vnc_password_and_resize() -> None:
    _setup_cloud_vps(101)
    pw = respx.put(f"{BASE}/vps/cloud/101/vnc-password").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    resize = respx.post(f"{BASE}/vps/cloud/101/resize").mock(
        return_value=httpx.Response(200, json=_ok({"queued": True}))
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(101)
        vps.vnc_password("new-password")
        assert vps.resize(instance_size="large") == {"queued": True}
    assert pw.called and resize.called


@respx.mock
def test_cloud_boot_order_validates_value() -> None:
    _setup_cloud_vps(102)
    valid = respx.put(f"{BASE}/vps/cloud/102/boot-order").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(102)
        vps.boot_order("cda")
        with pytest.raises(ValueError, match="cda|dca"):
            vps.boot_order("xyz")  # type: ignore[arg-type]
    assert valid.called


@respx.mock
def test_cloud_ipv6_enable() -> None:
    _setup_cloud_vps(103)
    route = respx.post(f"{BASE}/vps/cloud/103/ipv6").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        c.vps.get(103).ipv6_enable()
    assert route.called


# ── shared cancel ──────────────────────────────────────────────────────


@respx.mock
def test_cancel_works_on_proxmox() -> None:
    _setup_proxmox_vps(110)
    route = respx.post(f"{BASE}/vps/proxmox/110/cancel").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        c.vps.get(110).cancel(type="Immediate", reason="not needed")
    assert route.called
    body = route.calls.last.request.read()
    assert b"Immediate" in body and b"not needed" in body


@respx.mock
def test_cancel_works_on_cloud() -> None:
    _setup_cloud_vps(111)
    route = respx.post(f"{BASE}/vps/cloud/111/cancel").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    with Client(api_key="x", api_secret="y") as c:
        c.vps.get(111).cancel(type="End of Billing Period")
    assert route.called


# ── async coverage (representative) ────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_proxmox_snapshots_round_trip() -> None:
    respx.get(f"{BASE}/account/services/200").mock(
        return_value=httpx.Response(200, json=_service_payload(200, backend="proxmox"))
    )
    respx.get(f"{BASE}/vps/proxmox/200/snapshots").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_list("snapshots", [{"name": "x", "description": None, "created_at": None}]),
        )
    )
    create = respx.post(f"{BASE}/vps/proxmox/200/snapshots").mock(
        return_value=httpx.Response(200, json=_ok({"name": "y"}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(200)
        snaps = await vps.snapshots.list()
        assert len(snaps) == 1
        new = await vps.snapshots.create("y")
        assert new.name == "y"
    assert create.called


@pytest.mark.asyncio
@respx.mock
async def test_async_cloud_images_list() -> None:
    respx.get(f"{BASE}/account/services/210").mock(
        return_value=httpx.Response(200, json=_service_payload(210, backend="cloud"))
    )
    # Account-level — same shape as the sync test
    respx.get(f"{BASE}/vps/cloud/images").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_list("images", [{"id": 1, "name": "img"}]),
        )
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        images = await (await c.vps.get(210)).images.list()
    assert len(images) == 1


@pytest.mark.asyncio
@respx.mock
async def test_async_backend_not_supported_on_mismatch() -> None:
    respx.get(f"{BASE}/account/services/220").mock(
        return_value=httpx.Response(200, json=_service_payload(220, backend="cloud"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(220)
        with pytest.raises(BackendNotSupported):
            vps.snapshots  # noqa: B018


@pytest.mark.asyncio
@respx.mock
async def test_async_inline_proxmox_resources_method() -> None:
    respx.get(f"{BASE}/account/services/230").mock(
        return_value=httpx.Response(200, json=_service_payload(230, backend="proxmox"))
    )
    respx.get(f"{BASE}/vps/proxmox/230/resources").mock(
        return_value=httpx.Response(200, json=_ok({"cpu": 5.0}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(230)
        assert (await vps.resources()) == {"cpu": 5.0}


@pytest.mark.asyncio
@respx.mock
async def test_async_cloud_rdns_set() -> None:
    respx.get(f"{BASE}/account/services/240").mock(
        return_value=httpx.Response(200, json=_service_payload(240, backend="cloud"))
    )
    route = respx.put(f"{BASE}/vps/cloud/rdns/1.2.3.4").mock(
        return_value=httpx.Response(200, json=_ok({"hostname": "h"}))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(240)
        result = await vps.rdns.set("1.2.3.4", "h")
    assert route.called
    assert result == {"hostname": "h"}


@pytest.mark.asyncio
@respx.mock
async def test_async_cancel() -> None:
    respx.get(f"{BASE}/account/services/250").mock(
        return_value=httpx.Response(200, json=_service_payload(250, backend="proxmox"))
    )
    route = respx.post(f"{BASE}/vps/proxmox/250/cancel").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(250)
        await vps.cancel(type="Immediate")
    assert route.called
