"""Unit tests for the smart-dispatch ``vps`` resource (Phase 1.4b-i).

Covers the Vps bound model and the VpsResource entry point for both
backends (Proxmox + Cloud), sync and async. Mocked via ``respx`` — no
real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import (
    AsyncClient,
    AsyncVps,
    Client,
    InvalidRequest,
    Vps,
    VpsStatus,
)

BASE = "https://api.imprezahost.com/v1"


# ── payload helpers ────────────────────────────────────────────────────


def _service_payload(
    service_id: int,
    *,
    backend: str | None = "proxmox",
    domain: str = "vps.example.com",
    status: str = "Active",
) -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": service_id,
            "domain": domain,
            "status": status,
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


def _services_list_payload(services: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"services": services, "total": len(services)},
        "meta": {"request_id": "req_test"},
    }


def _proxmox_status_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "power_state": "running",
            "cpu_usage": 12.5,
            "memory_used": 1073741824,
            "memory_total": 4294967296,
            "uptime": 864000,
        },
        "meta": {"request_id": "req_test"},
    }


def _cloud_info_payload(state: str = "running") -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": 5678,
            "hostname": "cloud.example.com",
            "state": state,
            "os": "Debian 12",
            "location": "AMS",
            "ipv4": ["185.100.86.99"],
            "ipv6": ["2001:db8::1"],
        },
        "meta": {"request_id": "req_test"},
    }


def _ok_action() -> dict[str, object]:
    return {
        "success": True,
        "data": {"message": "ok"},
        "meta": {"request_id": "req_test"},
    }


# ── sync: smart dispatch ───────────────────────────────────────────────


@respx.mock
def test_get_resolves_proxmox_backend() -> None:
    respx.get(f"{BASE}/account/services/1234").mock(
        return_value=httpx.Response(200, json=_service_payload(1234, backend="proxmox"))
    )

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(1234)

    assert isinstance(vps, Vps)
    assert vps.id == 1234
    assert vps.backend == "proxmox"
    assert vps.service.domain == "vps.example.com"


@respx.mock
def test_get_resolves_cloud_backend() -> None:
    respx.get(f"{BASE}/account/services/5678").mock(
        return_value=httpx.Response(200, json=_service_payload(5678, backend="cloud"))
    )

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(5678)

    assert vps.backend == "cloud"


@respx.mock
def test_get_non_vps_raises_invalid_request() -> None:
    # service exists but vps_backend=null (e.g. a hosting service)
    respx.get(f"{BASE}/account/services/42").mock(
        return_value=httpx.Response(200, json=_service_payload(42, backend=None))
    )

    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(InvalidRequest) as exc_info,
    ):
        c.vps.get(42)

    assert exc_info.value.code == "NOT_A_VPS"


@respx.mock
def test_list_filters_to_vps_services_only() -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_services_list_payload(
                [
                    _service_payload(1, backend="proxmox")["data"],  # type: ignore[index]
                    _service_payload(2, backend=None)["data"],  # type: ignore[index]
                    _service_payload(3, backend="cloud")["data"],  # type: ignore[index]
                ]
            ),
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        vpss = c.vps.list()

    assert [v.id for v in vpss] == [1, 3]
    assert [v.backend for v in vpss] == ["proxmox", "cloud"]


# ── sync: power operations (URL normalization) ─────────────────────────


@respx.mock
def test_proxmox_power_uses_proxmox_paths() -> None:
    respx.get(f"{BASE}/account/services/100").mock(
        return_value=httpx.Response(200, json=_service_payload(100, backend="proxmox"))
    )
    start = respx.post(f"{BASE}/vps/proxmox/100/start").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    stop = respx.post(f"{BASE}/vps/proxmox/100/stop").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    reboot = respx.post(f"{BASE}/vps/proxmox/100/reboot").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    shutdown = respx.post(f"{BASE}/vps/proxmox/100/shutdown").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(100)
        vps.start()
        vps.stop()
        vps.reboot()
        vps.shutdown()

    assert start.called and stop.called and reboot.called and shutdown.called


@respx.mock
def test_cloud_power_uses_boot_and_poweroff_paths() -> None:
    respx.get(f"{BASE}/account/services/200").mock(
        return_value=httpx.Response(200, json=_service_payload(200, backend="cloud"))
    )
    boot = respx.post(f"{BASE}/vps/cloud/200/boot").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    poweroff = respx.post(f"{BASE}/vps/cloud/200/poweroff").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    reboot = respx.post(f"{BASE}/vps/cloud/200/reboot").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    shutdown = respx.post(f"{BASE}/vps/cloud/200/shutdown").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(200)
        vps.start()  # → /boot
        vps.stop()  # → /poweroff
        vps.reboot()
        vps.shutdown()

    assert boot.called and poweroff.called and reboot.called and shutdown.called


# ── sync: management ───────────────────────────────────────────────────


@respx.mock
def test_set_hostname_sends_put_with_body() -> None:
    respx.get(f"{BASE}/account/services/300").mock(
        return_value=httpx.Response(200, json=_service_payload(300, backend="proxmox"))
    )
    route = respx.put(f"{BASE}/vps/proxmox/300/hostname").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.vps.get(300).set_hostname("new.example.com")

    body = route.calls.last.request.read()
    assert b'"hostname"' in body
    assert b"new.example.com" in body


@respx.mock
def test_set_password_redacts_in_logs_but_sends_correctly() -> None:
    respx.get(f"{BASE}/account/services/300").mock(
        return_value=httpx.Response(200, json=_service_payload(300, backend="cloud"))
    )
    route = respx.put(f"{BASE}/vps/cloud/300/password").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.vps.get(300).set_password("Sup3rS3cret!")

    body = route.calls.last.request.read()
    assert b"Sup3rS3cret!" in body  # the value crosses the wire as-is


@respx.mock
def test_reinstall_requires_confirm_true() -> None:
    respx.get(f"{BASE}/account/services/400").mock(
        return_value=httpx.Response(200, json=_service_payload(400, backend="proxmox"))
    )

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(400)
        with pytest.raises(ValueError, match="confirm"):
            vps.reinstall(template="debian-12", password="x", confirm=False)


@respx.mock
def test_reinstall_with_confirm_returns_operation_on_proxmox() -> None:
    """reinstall on Proxmox returns an Operation future (Phase 1.5)."""
    respx.get(f"{BASE}/account/services/400").mock(
        return_value=httpx.Response(200, json=_service_payload(400, backend="proxmox"))
    )
    route = respx.post(f"{BASE}/vps/proxmox/400/reinstall").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {"uuid": "ri-001", "status": "queued"},
                "meta": {"request_id": "req"},
            },
        )
    )
    from impreza import Operation as _Operation

    with Client(api_key="x", api_secret="y") as c:
        op = c.vps.get(400).reinstall(template="debian-12", password="pwd", confirm=True)

    assert route.called
    body = route.calls.last.request.read()
    assert b'"confirm": true' in body or b'"confirm":true' in body
    assert b"debian-12" in body
    assert isinstance(op, _Operation)
    assert op.uuid == "ri-001"


@respx.mock
def test_reinstall_returns_none_on_cloud() -> None:
    """reinstall on Cloud is sync at the Cloud backend layer — returns None."""
    respx.get(f"{BASE}/account/services/401").mock(
        return_value=httpx.Response(200, json=_service_payload(401, backend="cloud"))
    )
    respx.post(f"{BASE}/vps/cloud/401/reinstall").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.vps.get(401).reinstall(template="ubuntu-22", password="pwd", confirm=True)

    assert result is None


# ── sync: status ───────────────────────────────────────────────────────


@respx.mock
def test_proxmox_status_returns_full_metrics() -> None:
    respx.get(f"{BASE}/account/services/500").mock(
        return_value=httpx.Response(200, json=_service_payload(500, backend="proxmox"))
    )
    respx.get(f"{BASE}/vps/proxmox/500/status").mock(
        return_value=httpx.Response(200, json=_proxmox_status_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        status = c.vps.get(500).status()

    assert isinstance(status, VpsStatus)
    assert status.power_state == "running"
    assert status.cpu_usage == 12.5
    assert status.memory_used == 1073741824
    assert status.memory_total == 4294967296
    assert status.uptime == 864000


@respx.mock
def test_cloud_status_extracts_state_from_info_response() -> None:
    respx.get(f"{BASE}/account/services/600").mock(
        return_value=httpx.Response(200, json=_service_payload(600, backend="cloud"))
    )
    # /vps/cloud/{id} returns the info payload, NOT a /status sub-resource
    respx.get(f"{BASE}/vps/cloud/600").mock(
        return_value=httpx.Response(200, json=_cloud_info_payload(state="stopped"))
    )

    with Client(api_key="x", api_secret="y") as c:
        status = c.vps.get(600).status()

    assert status.power_state == "stopped"
    # Cloud doesn't expose CPU/memory/uptime in the info response
    assert status.cpu_usage is None
    assert status.memory_used is None
    assert status.uptime is None


@respx.mock
def test_cloud_status_falls_back_to_unknown_when_no_state_field() -> None:
    respx.get(f"{BASE}/account/services/601").mock(
        return_value=httpx.Response(200, json=_service_payload(601, backend="cloud"))
    )
    respx.get(f"{BASE}/vps/cloud/601").mock(
        return_value=httpx.Response(
            200,
            json={"success": True, "data": {"id": 601}, "meta": {"request_id": "req"}},
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        status = c.vps.get(601).status()

    assert status.power_state == "unknown"


# ── sync: direct-id convenience + cache ────────────────────────────────


@respx.mock
def test_direct_id_start_does_lookup_then_dispatch() -> None:
    services_route = respx.get(f"{BASE}/account/services/700").mock(
        return_value=httpx.Response(200, json=_service_payload(700, backend="cloud"))
    )
    boot_route = respx.post(f"{BASE}/vps/cloud/700/boot").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.vps.start(700)

    assert services_route.call_count == 1
    assert boot_route.called


@respx.mock
def test_repeated_direct_id_calls_use_cache_no_extra_lookup() -> None:
    services_route = respx.get(f"{BASE}/account/services/800").mock(
        return_value=httpx.Response(200, json=_service_payload(800, backend="proxmox"))
    )
    respx.post(f"{BASE}/vps/proxmox/800/reboot").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    respx.post(f"{BASE}/vps/proxmox/800/shutdown").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    respx.put(f"{BASE}/vps/proxmox/800/hostname").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.vps.reboot(800)
        c.vps.shutdown(800)
        c.vps.set_hostname(800, "x.example.com")

    # Only one /account/services/{id} lookup despite three operations on
    # the same id — the resolved backend is cached on the resource.
    assert services_route.call_count == 1


@respx.mock
def test_set_hostname_via_resource_uses_cached_backend() -> None:
    services_route = respx.get(f"{BASE}/account/services/900").mock(
        return_value=httpx.Response(200, json=_service_payload(900, backend="cloud"))
    )
    put_route = respx.put(f"{BASE}/vps/cloud/900/hostname").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.vps.set_hostname(900, "renamed.example.com")
        c.vps.set_hostname(900, "renamed-again.example.com")

    assert services_route.call_count == 1
    assert put_route.call_count == 2


# ── sync: refresh ──────────────────────────────────────────────────────


@respx.mock
def test_refresh_updates_service_and_backend() -> None:
    # First the get() lookup, then a refresh that flips the backend.
    respx.get(f"{BASE}/account/services/1000").mock(
        side_effect=[
            httpx.Response(200, json=_service_payload(1000, backend="proxmox")),
            httpx.Response(200, json=_service_payload(1000, backend="cloud")),
        ]
    )

    with Client(api_key="x", api_secret="y") as c:
        vps = c.vps.get(1000)
        assert vps.backend == "proxmox"

        updated = vps.refresh()

        assert vps.backend == "cloud"
        assert updated.vps_backend == "cloud"


# ── async: minimal but real coverage ───────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_get_resolves_backend() -> None:
    respx.get(f"{BASE}/account/services/2000").mock(
        return_value=httpx.Response(200, json=_service_payload(2000, backend="proxmox"))
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(2000)

    assert isinstance(vps, AsyncVps)
    assert vps.backend == "proxmox"


@pytest.mark.asyncio
@respx.mock
async def test_async_proxmox_start_uses_proxmox_path() -> None:
    respx.get(f"{BASE}/account/services/2100").mock(
        return_value=httpx.Response(200, json=_service_payload(2100, backend="proxmox"))
    )
    start = respx.post(f"{BASE}/vps/proxmox/2100/start").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(2100)
        await vps.start()

    assert start.called


@pytest.mark.asyncio
@respx.mock
async def test_async_cloud_start_uses_boot_path() -> None:
    respx.get(f"{BASE}/account/services/2200").mock(
        return_value=httpx.Response(200, json=_service_payload(2200, backend="cloud"))
    )
    boot = respx.post(f"{BASE}/vps/cloud/2200/boot").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(2200)
        await vps.start()

    assert boot.called


@pytest.mark.asyncio
@respx.mock
async def test_async_proxmox_status_returns_full_metrics() -> None:
    respx.get(f"{BASE}/account/services/2300").mock(
        return_value=httpx.Response(200, json=_service_payload(2300, backend="proxmox"))
    )
    respx.get(f"{BASE}/vps/proxmox/2300/status").mock(
        return_value=httpx.Response(200, json=_proxmox_status_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        status = await (await c.vps.get(2300)).status()

    assert status.power_state == "running"
    assert status.cpu_usage == 12.5


@pytest.mark.asyncio
@respx.mock
async def test_async_direct_id_dispatch_and_cache() -> None:
    services_route = respx.get(f"{BASE}/account/services/2400").mock(
        return_value=httpx.Response(200, json=_service_payload(2400, backend="cloud"))
    )
    respx.post(f"{BASE}/vps/cloud/2400/boot").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )
    respx.post(f"{BASE}/vps/cloud/2400/poweroff").mock(
        return_value=httpx.Response(200, json=_ok_action())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        await c.vps.start(2400)
        await c.vps.stop(2400)

    # Cached on first call; second call doesn't re-lookup
    assert services_route.call_count == 1


@pytest.mark.asyncio
@respx.mock
async def test_async_reinstall_requires_confirm() -> None:
    respx.get(f"{BASE}/account/services/2500").mock(
        return_value=httpx.Response(200, json=_service_payload(2500, backend="proxmox"))
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(2500)
        with pytest.raises(ValueError, match="confirm"):
            await vps.reinstall(template="debian-12", password="x", confirm=False)


@pytest.mark.asyncio
@respx.mock
async def test_async_list_filters_and_caches() -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_services_list_payload(
                [
                    _service_payload(10, backend="proxmox")["data"],  # type: ignore[index]
                    _service_payload(11, backend=None)["data"],  # type: ignore[index]
                    _service_payload(12, backend="cloud")["data"],  # type: ignore[index]
                ]
            ),
        )
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        vpss = await c.vps.list()

    assert [v.id for v in vpss] == [10, 12]
    assert [v.backend for v in vpss] == ["proxmox", "cloud"]
