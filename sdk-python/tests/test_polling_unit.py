"""Unit tests for the action-polling layer (Phase 1.5).

Covers ``Operation`` and ``AsyncOperation``: state predicates,
``refresh()`` round-trips, ``wait()`` polling-to-completion,
``OperationTimeout`` on stuck queues, and ``OperationFailed`` on
terminal failure states.

Mocked via respx — no real API call. ``poll_interval`` is set very low
in tests so the suite runs quickly.
"""

from __future__ import annotations

import asyncio

import httpx
import pytest
import respx

from impreza import (
    AsyncClient,
    AsyncOperation,
    Client,
    Operation,
    OperationFailed,
    OperationTimeout,
)
from impreza._polling import build_async_operation, build_operation
from impreza.exceptions import ApiError

BASE = "https://api.imprezahost.com/v1"


def _ok_op(
    uuid: str,
    status: str,
    *,
    progress: int | float | None = None,
    error: str | None = None,
) -> dict[str, object]:
    data: dict[str, object] = {"uuid": uuid, "status": status}
    if progress is not None:
        data["progress"] = progress
    if error is not None:
        data["error"] = error
    return {"success": True, "data": data, "meta": {"request_id": "req_test"}}


# ── construction ──────────────────────────────────────────────────────


def test_build_operation_validates_uuid_present() -> None:
    """If the upstream payload has no `uuid`, build_operation raises ApiError."""
    http_stub = object()  # never called — error raises before any I/O
    payload = {"data": {"status": "running"}, "meta": {}}
    with pytest.raises(ApiError, match="uuid"):
        build_operation(http_stub, 1, payload)  # type: ignore[arg-type]


def test_build_operation_validates_status_present() -> None:
    http_stub = object()
    payload = {"data": {"uuid": "u1"}, "meta": {}}
    with pytest.raises(ApiError, match="status"):
        build_operation(http_stub, 1, payload)  # type: ignore[arg-type]


def test_operation_exposes_attributes_from_snapshot() -> None:
    """Operation proxies attribute access to its underlying VpsOperation."""
    with Client(api_key="x", api_secret="y") as c:
        op = build_operation(
            c._http,  # noqa: SLF001
            42,
            _ok_op("u-test", "running", progress=33),
        )
    assert op.uuid == "u-test"
    assert op.status == "running"
    assert op.progress == 33
    assert op.service_id == 42
    assert "u-test" in repr(op)
    assert "running" in repr(op)


# ── status predicates ─────────────────────────────────────────────────


@pytest.mark.parametrize(
    ("status", "is_done", "is_success", "is_failure"),
    [
        ("queued", False, False, False),
        ("running", False, False, False),
        ("completed", True, True, False),
        ("complete", True, True, False),
        ("success", True, True, False),
        ("succeeded", True, True, False),
        ("done", True, True, False),
        ("failed", True, False, True),
        ("cancelled", True, False, True),
        ("canceled", True, False, True),
        ("error", True, False, True),
    ],
)
def test_status_predicates_normalize(
    status: str, is_done: bool, is_success: bool, is_failure: bool
) -> None:
    with Client(api_key="x", api_secret="y") as c:
        op = build_operation(c._http, 1, _ok_op("u", status))  # noqa: SLF001
    assert op.is_done() is is_done
    assert op.is_success() is is_success
    assert op.is_failure() is is_failure


def test_status_predicates_are_case_insensitive() -> None:
    """Some upstreams emit uppercase ('COMPLETED') — predicates handle it."""
    with Client(api_key="x", api_secret="y") as c:
        op = build_operation(c._http, 1, _ok_op("u", "COMPLETED"))  # noqa: SLF001
    assert op.is_done() and op.is_success()


# ── sync wait() ───────────────────────────────────────────────────────


@respx.mock
def test_wait_polls_until_completion() -> None:
    """A queue that goes queued → running → completed terminates wait()."""
    respx.get(f"{BASE}/vps/proxmox/100/operations/u-001").mock(
        side_effect=[
            httpx.Response(200, json=_ok_op("u-001", "running", progress=50)),
            httpx.Response(200, json=_ok_op("u-001", "completed", progress=100)),
        ]
    )
    with Client(api_key="x", api_secret="y") as c:
        op = build_operation(
            c._http,  # noqa: SLF001
            100,
            _ok_op("u-001", "queued"),
        )
        result = op.wait(poll_interval=0.01, timeout=2.0)

    assert result is op
    assert op.status == "completed"
    assert op.progress == 100


@respx.mock
def test_wait_returns_immediately_if_already_done() -> None:
    """No polling happens if the snapshot is already in a terminal state."""
    route = respx.get(f"{BASE}/vps/proxmox/100/operations/u-002")
    with Client(api_key="x", api_secret="y") as c:
        op = build_operation(
            c._http,  # noqa: SLF001
            100,
            _ok_op("u-002", "completed", progress=100),
        )
        op.wait(poll_interval=0.01)
    assert not route.called


@respx.mock
def test_wait_raises_operation_failed_on_failure_status() -> None:
    respx.get(f"{BASE}/vps/proxmox/100/operations/u-003").mock(
        return_value=httpx.Response(
            200, json=_ok_op("u-003", "failed", error="VM did not respond")
        )
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(OperationFailed) as exc_info,
    ):
        op = build_operation(
            c._http,  # noqa: SLF001
            100,
            _ok_op("u-003", "running"),
        )
        op.wait(poll_interval=0.01, timeout=2.0)

    err = exc_info.value
    assert err.operation.status == "failed"  # type: ignore[attr-defined]
    assert "VM did not respond" in str(err)


@respx.mock
def test_wait_raises_timeout_when_queue_doesnt_finish() -> None:
    """A queue stuck in 'running' triggers OperationTimeout after timeout."""
    respx.get(f"{BASE}/vps/proxmox/100/operations/u-004").mock(
        return_value=httpx.Response(200, json=_ok_op("u-004", "running"))
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(OperationTimeout) as exc_info,
    ):
        op = build_operation(
            c._http,  # noqa: SLF001
            100,
            _ok_op("u-004", "queued"),
        )
        op.wait(poll_interval=0.05, timeout=0.15)
    assert exc_info.value.timeout == 0.15
    assert "u-004" in str(exc_info.value)


def test_wait_rejects_zero_or_negative_poll_interval() -> None:
    with Client(api_key="x", api_secret="y") as c:
        op = build_operation(c._http, 1, _ok_op("u", "running"))  # noqa: SLF001
        with pytest.raises(ValueError, match="poll_interval"):
            op.wait(poll_interval=0)


@respx.mock
def test_refresh_returns_self_with_updated_state() -> None:
    respx.get(f"{BASE}/vps/proxmox/100/operations/u-005").mock(
        return_value=httpx.Response(200, json=_ok_op("u-005", "running", progress=70))
    )
    with Client(api_key="x", api_secret="y") as c:
        op = build_operation(
            c._http,  # noqa: SLF001
            100,
            _ok_op("u-005", "queued"),
        )
        same = op.refresh()

    assert same is op
    assert op.status == "running"
    assert op.progress == 70


# ── async wait() ──────────────────────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_wait_polls_until_completion() -> None:
    respx.get(f"{BASE}/vps/proxmox/200/operations/au-001").mock(
        side_effect=[
            httpx.Response(200, json=_ok_op("au-001", "running")),
            httpx.Response(200, json=_ok_op("au-001", "completed")),
        ]
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        op = build_async_operation(
            c._http,  # noqa: SLF001
            200,
            _ok_op("au-001", "queued"),
        )
        result = await op.wait(poll_interval=0.01, timeout=2.0)

    assert result is op
    assert isinstance(op, AsyncOperation)
    assert op.is_success()


@pytest.mark.asyncio
@respx.mock
async def test_async_wait_raises_operation_failed() -> None:
    respx.get(f"{BASE}/vps/proxmox/200/operations/au-002").mock(
        return_value=httpx.Response(200, json=_ok_op("au-002", "failed", error="boom"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        op = build_async_operation(
            c._http,  # noqa: SLF001
            200,
            _ok_op("au-002", "running"),
        )
        with pytest.raises(OperationFailed) as exc_info:
            await op.wait(poll_interval=0.01, timeout=2.0)
    assert "boom" in str(exc_info.value)


@pytest.mark.asyncio
@respx.mock
async def test_async_wait_raises_timeout() -> None:
    respx.get(f"{BASE}/vps/proxmox/200/operations/au-003").mock(
        return_value=httpx.Response(200, json=_ok_op("au-003", "running"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        op = build_async_operation(
            c._http,  # noqa: SLF001
            200,
            _ok_op("au-003", "queued"),
        )
        with pytest.raises(OperationTimeout):
            await op.wait(poll_interval=0.05, timeout=0.15)


@pytest.mark.asyncio
@respx.mock
async def test_async_refresh_updates_state() -> None:
    respx.get(f"{BASE}/vps/proxmox/200/operations/au-004").mock(
        return_value=httpx.Response(200, json=_ok_op("au-004", "completed"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        op = build_async_operation(
            c._http,  # noqa: SLF001
            200,
            _ok_op("au-004", "queued"),
        )
        await op.refresh()
    assert op.status == "completed"


# ── integration: end-to-end via the real resource path ───────────────


@respx.mock
def test_snapshot_rollback_returns_operation_and_can_wait() -> None:
    """The actual UX: vps.snapshots.rollback().wait() — full round-trip."""
    # Service lookup (smart dispatch)
    respx.get(f"{BASE}/account/services/300").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "id": 300,
                    "domain": "vps.example.com",
                    "status": "Active",
                    "product": "x",
                    "vps_backend": "proxmox",
                },
                "meta": {"request_id": "req"},
            },
        )
    )
    # Rollback returns the queued operation
    respx.post(f"{BASE}/vps/proxmox/300/snapshots/pre-update/rollback").mock(
        return_value=httpx.Response(200, json=_ok_op("rb-002", "queued"))
    )
    # Polling shows running, then completed
    respx.get(f"{BASE}/vps/proxmox/300/operations/rb-002").mock(
        side_effect=[
            httpx.Response(200, json=_ok_op("rb-002", "running")),
            httpx.Response(200, json=_ok_op("rb-002", "completed")),
        ]
    )

    with Client(api_key="x", api_secret="y") as c:
        op = c.vps.get(300).snapshots.rollback("pre-update")
        assert isinstance(op, Operation)
        assert op.status == "queued"
        op.wait(poll_interval=0.01, timeout=2.0)

    assert op.is_success()


@respx.mock
def test_migrate_returns_operation_with_target_in_body() -> None:
    respx.get(f"{BASE}/account/services/301").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "id": 301,
                    "domain": "x",
                    "status": "Active",
                    "product": "x",
                    "vps_backend": "proxmox",
                },
                "meta": {"request_id": "req"},
            },
        )
    )
    route = respx.post(f"{BASE}/vps/proxmox/301/migrate").mock(
        return_value=httpx.Response(200, json=_ok_op("mg-002", "queued"))
    )
    with Client(api_key="x", api_secret="y") as c:
        op = c.vps.get(301).migrate(target="dc-eu-1")
    assert isinstance(op, Operation)
    assert op.uuid == "mg-002"
    assert b"dc-eu-1" in route.calls.last.request.read()


# ── async smoke through the full resource path ───────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_backup_create_returns_async_operation() -> None:
    respx.get(f"{BASE}/account/services/400").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "id": 400,
                    "domain": "x",
                    "status": "Active",
                    "product": "x",
                    "vps_backend": "proxmox",
                },
                "meta": {"request_id": "req"},
            },
        )
    )
    respx.post(f"{BASE}/vps/proxmox/400/backups").mock(
        return_value=httpx.Response(200, json=_ok_op("bk-async-001", "queued"))
    )
    respx.get(f"{BASE}/vps/proxmox/400/operations/bk-async-001").mock(
        return_value=httpx.Response(200, json=_ok_op("bk-async-001", "completed"))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        vps = await c.vps.get(400)
        op = await vps.backups.create()
        assert isinstance(op, AsyncOperation)
        await op.wait(poll_interval=0.01, timeout=2.0)
    assert op.is_success()


# Make sure asyncio is imported (used implicitly by pytest-asyncio); flake8/ruff
# would otherwise warn.
_ = asyncio
