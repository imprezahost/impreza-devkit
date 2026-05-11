"""Proxmox-only VPS sub-resources (Phase 1.4b-ii / 1.5).

Mounted on the :class:`~impreza.resources.vps.Vps` bound model as
``vps.snapshots``, ``vps.backups``, ``vps.backup_schedules``, and
``vps.operations`` properties. Each sub-resource raises
:class:`~impreza.exceptions.BackendNotSupported` if accessed on a
Cloud VPS.

Phase 1.5 update: the long-running mutating operations
(``backups.create``, ``backups.restore``, ``snapshots.rollback``)
now return an :class:`~impreza._polling.Operation` future instead of
a raw dict. Callers can ``op.wait(timeout=...)`` to block until the
upstream queue finishes, ``op.refresh()`` to poll once, or read
``op.status`` for the last-known state.

Sync and async variants share extractor helpers at the top.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from .._polling import (
    AsyncOperation,
    Operation,
    build_async_operation,
    build_operation,
)
from ..models.vps_extras import Backup, BackupSchedule, Snapshot, VpsOperation

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


# ── extractors / body builders (shared) ────────────────────────────────


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _items(payload: dict[str, object], key: str) -> list[dict[str, object]]:
    data = _data(payload)
    raw = data.get(key)
    if isinstance(raw, list):
        return [item for item in raw if isinstance(item, dict)]
    return []


def _extract_snapshots(payload: dict[str, object]) -> list[Snapshot]:
    return [Snapshot.model_validate(item) for item in _items(payload, "snapshots")]


def _extract_snapshot(payload: dict[str, object]) -> Snapshot:
    return Snapshot.model_validate(_data(payload))


def _extract_backups(payload: dict[str, object]) -> list[Backup]:
    return [Backup.model_validate(item) for item in _items(payload, "backups")]


def _extract_schedules(payload: dict[str, object]) -> list[BackupSchedule]:
    return [BackupSchedule.model_validate(item) for item in _items(payload, "schedules")]


def _extract_schedule(payload: dict[str, object]) -> BackupSchedule:
    return BackupSchedule.model_validate(_data(payload))


def _extract_operations(payload: dict[str, object]) -> list[VpsOperation]:
    return [VpsOperation.model_validate(item) for item in _items(payload, "operations")]


def _extract_operation(payload: dict[str, object]) -> VpsOperation:
    return VpsOperation.model_validate(_data(payload))


def _create_snapshot_body(name: str, description: str | None) -> dict[str, object]:
    body: dict[str, object] = {"name": name}
    if description is not None:
        body["description"] = description
    return body


def _create_schedule_body(
    *,
    dow: str,
    hour: int,
    minute: int,
    mode: str | None,
    compress: str | None,
) -> dict[str, object]:
    body: dict[str, object] = {"dow": dow, "hour": hour, "minute": minute}
    if mode is not None:
        body["mode"] = mode
    if compress is not None:
        body["compress"] = compress
    return body


def _migrate_body(target: str) -> dict[str, object]:
    return {"target": target}


def _cancel_body(*, type: str, reason: str | None) -> dict[str, object]:
    body: dict[str, object] = {"type": type}
    if reason is not None:
        body["reason"] = reason
    return body


# ── sync ───────────────────────────────────────────────────────────────


class ProxmoxSnapshotsResource:
    """Snapshots on a Proxmox VPS — ``vps.snapshots``."""

    def __init__(self, http: HttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/snapshots"

    def list(self) -> list[Snapshot]:
        return _extract_snapshots(self._http.get(self._root))

    def create(self, name: str, *, description: str | None = None) -> Snapshot:
        return _extract_snapshot(
            self._http.post(self._root, json=_create_snapshot_body(name, description))
        )

    def delete(self, name: str) -> None:
        self._http.delete(f"{self._root}/{name}")

    def rollback(self, name: str) -> Operation:
        """Roll the VM back to the snapshot. VM is stopped during rollback.

        Returns an :class:`Operation` future. Call ``.wait(timeout=...)``
        to block until rollback completes, or read ``.status`` for the
        queued state.
        """
        payload = self._http.post(f"{self._root}/{name}/rollback")
        return build_operation(self._http, self._sid, payload)


class ProxmoxBackupsResource:
    """Backups on a Proxmox VPS — ``vps.backups``."""

    def __init__(self, http: HttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/backups"

    def list(self) -> list[Backup]:
        return _extract_backups(self._http.get(self._root))

    def create(self) -> Operation:
        """Trigger a new backup. Subject to per-VM backup limits.

        Returns an :class:`Operation` future. Call ``.wait(timeout=...)``
        to block until the backup completes.
        """
        payload = self._http.post(self._root)
        return build_operation(self._http, self._sid, payload)

    def restore(self, backup_id: str | int) -> Operation:
        """Restore a backup. VM is stopped during restore.

        Returns an :class:`Operation` future. Call ``.wait(timeout=...)``
        to block until restore completes.
        """
        payload = self._http.post(f"{self._root}/{backup_id}/restore")
        return build_operation(self._http, self._sid, payload)

    def delete(self, backup_id: str | int) -> None:
        """Delete a backup. Protected backups cannot be deleted."""
        self._http.delete(f"{self._root}/{backup_id}")


class ProxmoxBackupSchedulesResource:
    """Backup schedules on a Proxmox VPS — ``vps.backup_schedules``."""

    def __init__(self, http: HttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/backup-schedules"

    def list(self) -> list[BackupSchedule]:
        return _extract_schedules(self._http.get(self._root))

    def create(
        self,
        *,
        dow: str,
        hour: int,
        minute: int,
        mode: str | None = None,
        compress: str | None = None,
    ) -> BackupSchedule:
        """Create a scheduled backup job.

        ``dow`` is a comma-separated day-of-week selector
        (``"mon,wed,fri"``). ``mode`` typically one of
        ``"snapshot"``, ``"suspend"``, ``"stop"``. ``compress`` typically
        ``"zstd"`` / ``"lzo"`` / ``"gzip"`` / ``"none"``.
        """
        return _extract_schedule(
            self._http.post(
                self._root,
                json=_create_schedule_body(
                    dow=dow, hour=hour, minute=minute, mode=mode, compress=compress
                ),
            )
        )

    def delete(self, schedule_id: str | int) -> None:
        self._http.delete(f"{self._root}/{schedule_id}")


class ProxmoxOperationsResource:
    """Async-operation queue on a Proxmox VPS — ``vps.operations``.

    Phase 1.5 will add ``op.wait()`` polling helpers built on this surface.
    """

    def __init__(self, http: HttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/operations"

    def list(self) -> list[VpsOperation]:
        return _extract_operations(self._http.get(self._root))

    def get(self, uuid: str) -> VpsOperation:
        return _extract_operation(self._http.get(f"{self._root}/{uuid}"))


# ── async ──────────────────────────────────────────────────────────────


class AsyncProxmoxSnapshotsResource:
    def __init__(self, http: AsyncHttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/snapshots"

    async def list(self) -> list[Snapshot]:
        return _extract_snapshots(await self._http.get(self._root))

    async def create(self, name: str, *, description: str | None = None) -> Snapshot:
        return _extract_snapshot(
            await self._http.post(self._root, json=_create_snapshot_body(name, description))
        )

    async def delete(self, name: str) -> None:
        await self._http.delete(f"{self._root}/{name}")

    async def rollback(self, name: str) -> AsyncOperation:
        payload = await self._http.post(f"{self._root}/{name}/rollback")
        return build_async_operation(self._http, self._sid, payload)


class AsyncProxmoxBackupsResource:
    def __init__(self, http: AsyncHttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/backups"

    async def list(self) -> list[Backup]:
        return _extract_backups(await self._http.get(self._root))

    async def create(self) -> AsyncOperation:
        payload = await self._http.post(self._root)
        return build_async_operation(self._http, self._sid, payload)

    async def restore(self, backup_id: str | int) -> AsyncOperation:
        payload = await self._http.post(f"{self._root}/{backup_id}/restore")
        return build_async_operation(self._http, self._sid, payload)

    async def delete(self, backup_id: str | int) -> None:
        await self._http.delete(f"{self._root}/{backup_id}")


class AsyncProxmoxBackupSchedulesResource:
    def __init__(self, http: AsyncHttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/backup-schedules"

    async def list(self) -> list[BackupSchedule]:
        return _extract_schedules(await self._http.get(self._root))

    async def create(
        self,
        *,
        dow: str,
        hour: int,
        minute: int,
        mode: str | None = None,
        compress: str | None = None,
    ) -> BackupSchedule:
        return _extract_schedule(
            await self._http.post(
                self._root,
                json=_create_schedule_body(
                    dow=dow, hour=hour, minute=minute, mode=mode, compress=compress
                ),
            )
        )

    async def delete(self, schedule_id: str | int) -> None:
        await self._http.delete(f"{self._root}/{schedule_id}")


class AsyncProxmoxOperationsResource:
    def __init__(self, http: AsyncHttpClient, service_id: int) -> None:
        self._http = http
        self._sid = service_id

    @property
    def _root(self) -> str:
        return f"/vps/proxmox/{self._sid}/operations"

    async def list(self) -> list[VpsOperation]:
        return _extract_operations(await self._http.get(self._root))

    async def get(self, uuid: str) -> VpsOperation:
        return _extract_operation(await self._http.get(f"{self._root}/{uuid}"))
