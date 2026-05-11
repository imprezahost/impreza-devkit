"""VPS resource — accessed via ``Client.vps`` and ``AsyncClient.vps``.

Phase 1.4b-i delivers the **smart-dispatch entry point** plus the
**common operations** shared by both VPS backends (Proxmox, Cloud):

* ``c.vps.get(service_id)`` resolves the backend via
  ``GET /account/services/{id}`` and returns a :class:`Vps` (or
  :class:`AsyncVps`) bound model carrying the backend identity.
* The bound model exposes the operations that exist on both
  backends: power (``start`` / ``stop`` / ``reboot`` / ``shutdown``),
  ``set_hostname``, ``set_password``, ``reinstall``, ``status``.
* ``c.vps.list()`` returns every VPS the client owns across both
  backends, normalized.
* Direct-ID convenience wrappers (``c.vps.start(service_id)`` etc.)
  exist for one-shot use; they cache the resolved backend on the
  resource so a follow-up call doesn't re-fetch the service.

Backend-specific surfaces (snapshots+backups+queue for Proxmox;
images+rescue+ISO+rDNS for Cloud) land in 1.4b-ii.

URL normalization for power operations:

==================  ========================  ===========================
Common method       Proxmox URL              Cloud URL
==================  ========================  ===========================
``start()``         ``/start``                ``/boot``
``shutdown()``      ``/shutdown``             ``/shutdown``
``reboot()``        ``/reboot``               ``/reboot``
``stop()``          ``/stop``                 ``/poweroff``
==================  ========================  ===========================

``set_hostname``, ``set_password`` and ``reinstall`` use identical
relative paths on both backends, so no normalization is needed there.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Literal

from .._polling import (
    AsyncOperation,
    Operation,
    build_async_operation,
    build_operation,
)
from ..exceptions import BackendNotSupported, InvalidRequest
from ..models.service import Service, VpsBackend
from ..models.vps import VpsStatus
from ..models.vps_extras import ConsoleUrl, SshConsole, VncCredentials
from .vps_cloud import (
    AsyncCloudImagesResource,
    AsyncCloudIsoResource,
    AsyncCloudRdnsResource,
    AsyncCloudRescueResource,
    AsyncCloudSshKeysResource,
    CloudImagesResource,
    CloudIsoResource,
    CloudRdnsResource,
    CloudRescueResource,
    CloudSshKeysResource,
)
from .vps_proxmox import (
    AsyncProxmoxBackupSchedulesResource,
    AsyncProxmoxBackupsResource,
    AsyncProxmoxOperationsResource,
    AsyncProxmoxSnapshotsResource,
    ProxmoxBackupSchedulesResource,
    ProxmoxBackupsResource,
    ProxmoxOperationsResource,
    ProxmoxSnapshotsResource,
)

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient

# Boot-order values accepted by the Cloud backend.
_VALID_BOOT_ORDER = {"cda", "dca"}


# ── extractors / helpers (shared between sync and async) ───────────────


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _service_from_payload(payload: dict[str, object]) -> Service:
    return Service.model_validate(_data(payload))


def _services_from_payload(payload: dict[str, object]) -> list[Service]:
    data = _data(payload)
    raw = data.get("services")
    items = raw if isinstance(raw, list) else []
    return [Service.model_validate(item) for item in items]


def _proxmox_status_from_payload(payload: dict[str, object]) -> VpsStatus:
    """Proxmox ``GET /vps/proxmox/{id}/status`` already matches VpsStatus."""
    return VpsStatus.model_validate(_data(payload))


def _cloud_status_from_payload(payload: dict[str, object]) -> VpsStatus:
    """Cloud ``GET /vps/cloud/{id}`` is a richer info object — extract the
    common status fields and let the rest fall through.

    The Cloud backend response shape commonly uses ``state`` (``running``,
    ``stopped``, ``rebooting``, ...) for the power state. We keep matching
    loose so renames upstream don't break clients.
    """
    data = _data(payload)
    candidates = ("power_state", "state", "status")
    power_state = ""
    for key in candidates:
        value = data.get(key)
        if isinstance(value, str) and value:
            power_state = value
            break
    return VpsStatus(
        power_state=power_state or "unknown",
        cpu_usage=_optional_float(data.get("cpu_usage")),
        memory_used=_optional_int(data.get("memory_used")),
        memory_total=_optional_int(data.get("memory_total")),
        uptime=_optional_int(data.get("uptime")),
    )


def _optional_int(value: object) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    return None


def _optional_float(value: object) -> float | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        return float(value)
    return None


def _power_path(backend: VpsBackend, action: Literal["start", "stop", "reboot", "shutdown"]) -> str:
    """Map a normalized power action to the backend's URL segment."""
    if backend == "proxmox":
        return action  # /start, /stop, /reboot, /shutdown — all identical names
    # cloud renames start→boot and stop→poweroff
    if action == "start":
        return "boot"
    if action == "stop":
        return "poweroff"
    return action  # /reboot, /shutdown match


def _reinstall_body(template: str, password: str, *, confirm: bool) -> dict[str, object]:
    if not confirm:
        raise ValueError(
            "reinstall is destructive — pass confirm=True to acknowledge data loss",
        )
    return {"template": template, "password": password, "confirm": True}


def _cancel_body(*, type: str, reason: str | None) -> dict[str, object]:
    body: dict[str, object] = {"type": type}
    if reason is not None:
        body["reason"] = reason
    return body


def _filter_vps_services(services: list[Service]) -> list[Service]:
    return [s for s in services if s.vps_backend is not None]


def _not_a_vps(service_id: int) -> InvalidRequest:
    return InvalidRequest(
        f"Service {service_id} is not a VPS or its backend is not recognized "
        f"(no vps_backend set on /account/services/{service_id}). Use "
        "Client.account.services.get() to inspect the service.",
        code="NOT_A_VPS",
        status_code=400,
    )


# ── sync ───────────────────────────────────────────────────────────────


class Vps:
    """A bound VPS — carries the backend identity so operations route correctly.

    Constructed by :class:`VpsResource`. Don't instantiate directly.

    Attributes:
        id: Service id (matches both ``/vps/proxmox/{id}`` and
            ``/vps/cloud/{id}`` URL conventions in our API).
        backend: ``"proxmox"`` or ``"cloud"``.
        service: the :class:`~impreza.models.service.Service` snapshot
            from the resolution call. Use :meth:`refresh` to update.
    """

    __slots__ = ("_http", "_resource", "id", "backend", "service")

    def __init__(
        self,
        http: HttpClient,
        resource: VpsResource,
        service: Service,
    ) -> None:
        if service.vps_backend is None:
            raise _not_a_vps(service.id)
        self._http = http
        self._resource = resource
        self.id: int = service.id
        self.backend: VpsBackend = service.vps_backend
        self.service: Service = service

    # ── read ───────────────────────────────────────────────────────────

    @property
    def _root(self) -> str:
        return f"/vps/{self.backend}/{self.id}"

    def status(self) -> VpsStatus:
        """Fetch live power state and metrics.

        Hits ``/vps/proxmox/{id}/status`` for Proxmox, ``/vps/cloud/{id}``
        for Cloud. The Cloud backend only populates ``power_state`` —
        CPU / memory / uptime are Proxmox-only today.
        """
        if self.backend == "proxmox":
            payload = self._http.get(f"{self._root}/status")
            return _proxmox_status_from_payload(payload)
        payload = self._http.get(self._root)
        return _cloud_status_from_payload(payload)

    def refresh(self) -> Service:
        """Re-fetch the underlying ``Service`` and update the cache.

        Useful after operations that mutate state (rename, reinstall) or
        when the SDK saw a stale 404 and you want to confirm the resource
        still exists.
        """
        payload = self._http.get(f"/account/services/{self.id}")
        service = _service_from_payload(payload)
        self._resource._cache_service(service)  # noqa: SLF001
        # Replace stored service. backend can change in theory if the
        # admin reassigned the service to a different server module.
        if service.vps_backend is None:
            raise _not_a_vps(self.id)
        self.service = service
        self.backend = service.vps_backend
        return service

    # ── power ──────────────────────────────────────────────────────────

    def start(self) -> None:
        """Boot a stopped VPS."""
        self._http.post(f"{self._root}/{_power_path(self.backend, 'start')}")

    def shutdown(self) -> None:
        """Graceful shutdown via ACPI."""
        self._http.post(f"{self._root}/{_power_path(self.backend, 'shutdown')}")

    def reboot(self) -> None:
        """Reboot."""
        self._http.post(f"{self._root}/{_power_path(self.backend, 'reboot')}")

    def stop(self) -> None:
        """Force-stop. May corrupt unwritten data — prefer :meth:`shutdown`."""
        self._http.post(f"{self._root}/{_power_path(self.backend, 'stop')}")

    # ── management ─────────────────────────────────────────────────────

    def set_hostname(self, hostname: str) -> None:
        """Change the VPS hostname."""
        self._http.put(f"{self._root}/hostname", json={"hostname": hostname})

    def set_password(self, password: str) -> None:
        """Reset the VPS root/admin password."""
        self._http.put(f"{self._root}/password", json={"password": password})

    def reinstall(
        self, *, template: str, password: str, confirm: bool
    ) -> Operation | None:
        """Reinstall the OS. Destructive — wipes everything.

        ``confirm=True`` is required to acknowledge data loss.

        Backend-specific return value (Phase 1.5):

        * **Proxmox**: returns an :class:`Operation` future. Call
          ``.wait(timeout=...)`` to block until reinstall completes.
        * **Cloud**: reinstall is fire-and-forget at the Cloud backend
          API layer — the upstream returns immediately with a
          synchronous status payload, so this method returns
          ``None``. Track progress by polling :meth:`status` instead.
        """
        payload = self._http.post(
            f"{self._root}/reinstall",
            json=_reinstall_body(template, password, confirm=confirm),
        )
        if self.backend == "proxmox":
            return build_operation(self._http, self.id, payload)
        return None

    def cancel(self, *, type: str, reason: str | None = None) -> None:
        """Submit a cancellation request. Works on both backends.

        ``type`` is one of ``"Immediate"`` or ``"End of Billing Period"``.
        """
        self._http.post(f"{self._root}/cancel", json=_cancel_body(type=type, reason=reason))

    # ── Proxmox-only sub-resources ─────────────────────────────────────

    @property
    def snapshots(self) -> ProxmoxSnapshotsResource:
        """List/create/delete/rollback Proxmox snapshots."""
        self._require_backend("proxmox", "snapshots")
        return ProxmoxSnapshotsResource(self._http, self.id)

    @property
    def backups(self) -> ProxmoxBackupsResource:
        """List/create/restore/delete Proxmox backups."""
        self._require_backend("proxmox", "backups")
        return ProxmoxBackupsResource(self._http, self.id)

    @property
    def backup_schedules(self) -> ProxmoxBackupSchedulesResource:
        """List/create/delete Proxmox backup schedules."""
        self._require_backend("proxmox", "backup_schedules")
        return ProxmoxBackupSchedulesResource(self._http, self.id)

    @property
    def operations(self) -> ProxmoxOperationsResource:
        """Track Proxmox async operations (reinstall, migrate, etc.)."""
        self._require_backend("proxmox", "operations")
        return ProxmoxOperationsResource(self._http, self.id)

    # ── Proxmox-only inline methods ────────────────────────────────────

    def info(self) -> dict[str, object]:
        """Full VM info — Proxmox-only.

        On Cloud, the same data is exposed by :meth:`refresh` (returns the
        Service) plus :meth:`status`. ``Vps.info`` is intentionally Proxmox-only
        because the Cloud info object is the same payload :meth:`status` already
        normalizes.
        """
        self._require_backend("proxmox", "info")
        return _data(self._http.get(self._root))

    def config(self) -> dict[str, object]:
        """Full Proxmox VM config (cores, sockets, memory, disks, boot)."""
        self._require_backend("proxmox", "config")
        return _data(self._http.get(f"{self._root}/config"))

    def pending(self) -> dict[str, object]:
        """Proxmox config changes pending a reboot."""
        self._require_backend("proxmox", "pending")
        return _data(self._http.get(f"{self._root}/pending"))

    def resources(self) -> dict[str, object]:
        """Live CPU / memory / disk / network resource usage."""
        self._require_backend("proxmox", "resources")
        return _data(self._http.get(f"{self._root}/resources"))

    def ips(self) -> dict[str, object]:
        """List every IP assigned to this VM with gateway / subnet."""
        self._require_backend("proxmox", "ips")
        return _data(self._http.get(f"{self._root}/ips"))

    def available_ips(self) -> dict[str, object]:
        """Unallocated IPv4 / IPv6 count for the location of this VM."""
        self._require_backend("proxmox", "available_ips")
        return _data(self._http.get(f"{self._root}/available-ips"))

    def templates(self) -> dict[str, object]:
        """Available OS templates for reinstall on this VM."""
        self._require_backend("proxmox", "templates")
        return _data(self._http.get(f"{self._root}/templates"))

    def locations(self) -> dict[str, object]:
        """Migration destinations available to this VM."""
        self._require_backend("proxmox", "locations")
        return _data(self._http.get(f"{self._root}/locations"))

    def console(self) -> ConsoleUrl:
        """noVNC console URL — open in a browser. Typically valid ~2h."""
        self._require_backend("proxmox", "console")
        return ConsoleUrl.model_validate(_data(self._http.get(f"{self._root}/console")))

    def console_ssh(self, *, password: str) -> SshConsole:
        """WebSocket SSH console credentials (encrypted_token + ws_url)."""
        self._require_backend("proxmox", "console_ssh")
        return SshConsole.model_validate(
            _data(self._http.post(f"{self._root}/console/ssh", json={"password": password}))
        )

    def network_reconfigure(self) -> dict[str, object]:
        """Apply pending network config (Guest Agent or reboot required)."""
        self._require_backend("proxmox", "network_reconfigure")
        return _data(self._http.post(f"{self._root}/network/reconfigure"))

    def migrate(self, *, target: str) -> Operation:
        """Migrate the VM to ``target`` (server_id or group_id). Async.

        Returns an :class:`Operation` future. Migration runs upstream as
        a long Proxmox queue job — call ``.wait(timeout=...)`` to block
        until complete (typically minutes, not seconds).
        """
        self._require_backend("proxmox", "migrate")
        payload = self._http.post(f"{self._root}/migrate", json={"target": target})
        return build_operation(self._http, self.id, payload)

    # No suspend()/unsuspend(): the customer-facing API retired the
    # Proxmox /suspend and /unsuspend routes on 2026-05-11. Impreza
    # service suspension (the billing-state operation) is staff-only;
    # the Proxmox VM freeze/resume pair was retired alongside it to
    # eliminate the semantic overlap. Customers needing to pause a
    # guest use shutdown; service wind-down goes through cancel().

    # ── Cloud-only sub-resources ───────────────────────────────────────

    @property
    def images(self) -> CloudImagesResource:
        """List/create/restore/delete Cloud VM images (saved snapshots)."""
        self._require_backend("cloud", "images")
        return CloudImagesResource(self._http, self.id)

    @property
    def rescue(self) -> CloudRescueResource:
        """Enable / disable rescue mode on a Cloud VPS."""
        self._require_backend("cloud", "rescue")
        return CloudRescueResource(self._http, self.id)

    @property
    def iso(self) -> CloudIsoResource:
        """Mount / unmount an ISO on a Cloud VPS."""
        self._require_backend("cloud", "iso")
        return CloudIsoResource(self._http, self.id)

    @property
    def rdns(self) -> CloudRdnsResource:
        """Get / set / delete reverse-DNS records (account-scoped at API)."""
        self._require_backend("cloud", "rdns")
        return CloudRdnsResource(self._http)

    @property
    def ssh_keys(self) -> CloudSshKeysResource:
        """List account SSH keys; assign one or more to this VPS."""
        self._require_backend("cloud", "ssh_keys")
        return CloudSshKeysResource(self._http, self.id)

    # ── Cloud-only inline methods ──────────────────────────────────────

    def vnc(self) -> VncCredentials:
        """VNC client credentials (host, port, password) for this Cloud VPS."""
        self._require_backend("cloud", "vnc")
        return VncCredentials.model_validate(_data(self._http.get(f"{self._root}/vnc")))

    def vnc_password(self, password: str) -> None:
        """Rotate the VNC password."""
        self._require_backend("cloud", "vnc_password")
        self._http.put(f"{self._root}/vnc-password", json={"password": password})

    def resize(self, *, instance_size: str) -> dict[str, object]:
        """Resize the VM to ``instance_size``. Reboot required to apply."""
        self._require_backend("cloud", "resize")
        return _data(self._http.post(f"{self._root}/resize", json={"instance_size": instance_size}))

    def boot_order(self, order: Literal["cda", "dca"]) -> None:
        """Set the boot order. Accepts ``"cda"`` or ``"dca"``."""
        self._require_backend("cloud", "boot_order")
        if order not in _VALID_BOOT_ORDER:
            raise ValueError(f"order must be one of {sorted(_VALID_BOOT_ORDER)}")
        self._http.put(f"{self._root}/boot-order", json={"bootorder": order})

    def ipv6_enable(self) -> None:
        """Enable IPv6 on the VM."""
        self._require_backend("cloud", "ipv6_enable")
        self._http.post(f"{self._root}/ipv6")

    # ── guards ─────────────────────────────────────────────────────────

    def _require_backend(self, expected: VpsBackend, op: str) -> None:
        if self.backend != expected:
            other = "cloud" if expected == "proxmox" else "proxmox"
            hint = (
                f"This VPS is on the {self.backend!r} backend; {op!r} is "
                f"only available on {expected!r}."
            )
            if self.backend == other:
                hint += f" See the {self.backend!r} surface for the equivalent."
            raise BackendNotSupported(self.backend, op, hint=hint)

    # ── repr ───────────────────────────────────────────────────────────

    def __repr__(self) -> str:
        domain = self.service.domain or "(no domain)"
        return f"Vps(id={self.id}, backend={self.backend!r}, domain={domain!r})"


class VpsResource:
    """Smart-dispatch entry point for VPS operations.

    The resource maintains an in-memory cache of resolved backends so
    repeated operations on the same service don't re-fetch
    ``/account/services/{id}``. The cache is populated on every
    successful resolution and invalidated when :meth:`refresh` on the
    bound model encounters a backend change.
    """

    def __init__(self, http: HttpClient) -> None:
        self._http = http
        self._cache: dict[int, VpsBackend] = {}

    # ── primary surface ────────────────────────────────────────────────

    def get(self, service_id: int) -> Vps:
        """Resolve a VPS by service id.

        Raises:
            ResourceNotFound: when no service with that id exists.
            InvalidRequest: when the service exists but is not a VPS
                (or its backend is not recognized).
        """
        payload = self._http.get(f"/account/services/{service_id}")
        service = _service_from_payload(payload)
        return Vps(self._http, self, service)

    def list(self) -> list[Vps]:
        """Return every VPS the authenticated client owns, both backends.

        Hits ``GET /account/services`` once and constructs bound models
        for the entries with a non-null ``vps_backend``. The resolved
        backends are cached on the resource.
        """
        payload = self._http.get("/account/services")
        services = _services_from_payload(payload)
        result: list[Vps] = []
        for service in _filter_vps_services(services):
            self._cache_service(service)
            result.append(Vps(self._http, self, service))
        return result

    # ── direct-id convenience (smart dispatch in one call) ─────────────

    def start(self, service_id: int) -> None:
        self._dispatch_power(service_id, "start")

    def shutdown(self, service_id: int) -> None:
        self._dispatch_power(service_id, "shutdown")

    def reboot(self, service_id: int) -> None:
        self._dispatch_power(service_id, "reboot")

    def stop(self, service_id: int) -> None:
        self._dispatch_power(service_id, "stop")

    def set_hostname(self, service_id: int, hostname: str) -> None:
        backend = self._resolve_backend(service_id)
        self._http.put(f"/vps/{backend}/{service_id}/hostname", json={"hostname": hostname})

    def set_password(self, service_id: int, password: str) -> None:
        backend = self._resolve_backend(service_id)
        self._http.put(f"/vps/{backend}/{service_id}/password", json={"password": password})

    def reinstall(
        self,
        service_id: int,
        *,
        template: str,
        password: str,
        confirm: bool,
    ) -> None:
        backend = self._resolve_backend(service_id)
        self._http.post(
            f"/vps/{backend}/{service_id}/reinstall",
            json=_reinstall_body(template, password, confirm=confirm),
        )

    # ── internals ──────────────────────────────────────────────────────

    def _dispatch_power(
        self,
        service_id: int,
        action: Literal["start", "stop", "reboot", "shutdown"],
    ) -> None:
        backend = self._resolve_backend(service_id)
        self._http.post(f"/vps/{backend}/{service_id}/{_power_path(backend, action)}")

    def _resolve_backend(self, service_id: int) -> VpsBackend:
        cached = self._cache.get(service_id)
        if cached is not None:
            return cached
        payload = self._http.get(f"/account/services/{service_id}")
        service = _service_from_payload(payload)
        if service.vps_backend is None:
            raise _not_a_vps(service_id)
        self._cache[service_id] = service.vps_backend
        return service.vps_backend

    def _cache_service(self, service: Service) -> None:
        if service.vps_backend is not None:
            self._cache[service.id] = service.vps_backend


# ── async ──────────────────────────────────────────────────────────────


class AsyncVps:
    """Async counterpart to :class:`Vps`."""

    __slots__ = ("_http", "_resource", "id", "backend", "service")

    def __init__(
        self,
        http: AsyncHttpClient,
        resource: AsyncVpsResource,
        service: Service,
    ) -> None:
        if service.vps_backend is None:
            raise _not_a_vps(service.id)
        self._http = http
        self._resource = resource
        self.id: int = service.id
        self.backend: VpsBackend = service.vps_backend
        self.service: Service = service

    @property
    def _root(self) -> str:
        return f"/vps/{self.backend}/{self.id}"

    async def status(self) -> VpsStatus:
        if self.backend == "proxmox":
            payload = await self._http.get(f"{self._root}/status")
            return _proxmox_status_from_payload(payload)
        payload = await self._http.get(self._root)
        return _cloud_status_from_payload(payload)

    async def refresh(self) -> Service:
        payload = await self._http.get(f"/account/services/{self.id}")
        service = _service_from_payload(payload)
        self._resource._cache_service(service)  # noqa: SLF001
        if service.vps_backend is None:
            raise _not_a_vps(self.id)
        self.service = service
        self.backend = service.vps_backend
        return service

    async def start(self) -> None:
        await self._http.post(f"{self._root}/{_power_path(self.backend, 'start')}")

    async def shutdown(self) -> None:
        await self._http.post(f"{self._root}/{_power_path(self.backend, 'shutdown')}")

    async def reboot(self) -> None:
        await self._http.post(f"{self._root}/{_power_path(self.backend, 'reboot')}")

    async def stop(self) -> None:
        await self._http.post(f"{self._root}/{_power_path(self.backend, 'stop')}")

    async def set_hostname(self, hostname: str) -> None:
        await self._http.put(f"{self._root}/hostname", json={"hostname": hostname})

    async def set_password(self, password: str) -> None:
        await self._http.put(f"{self._root}/password", json={"password": password})

    async def reinstall(
        self, *, template: str, password: str, confirm: bool
    ) -> AsyncOperation | None:
        """Reinstall the OS. See sync :meth:`Vps.reinstall` for backend-specific
        return semantics."""
        payload = await self._http.post(
            f"{self._root}/reinstall",
            json=_reinstall_body(template, password, confirm=confirm),
        )
        if self.backend == "proxmox":
            return build_async_operation(self._http, self.id, payload)
        return None

    async def cancel(self, *, type: str, reason: str | None = None) -> None:
        """Submit a cancellation request. Works on both backends."""
        await self._http.post(
            f"{self._root}/cancel", json=_cancel_body(type=type, reason=reason)
        )

    # ── Proxmox-only sub-resources ─────────────────────────────────────

    @property
    def snapshots(self) -> AsyncProxmoxSnapshotsResource:
        self._require_backend("proxmox", "snapshots")
        return AsyncProxmoxSnapshotsResource(self._http, self.id)

    @property
    def backups(self) -> AsyncProxmoxBackupsResource:
        self._require_backend("proxmox", "backups")
        return AsyncProxmoxBackupsResource(self._http, self.id)

    @property
    def backup_schedules(self) -> AsyncProxmoxBackupSchedulesResource:
        self._require_backend("proxmox", "backup_schedules")
        return AsyncProxmoxBackupSchedulesResource(self._http, self.id)

    @property
    def operations(self) -> AsyncProxmoxOperationsResource:
        self._require_backend("proxmox", "operations")
        return AsyncProxmoxOperationsResource(self._http, self.id)

    # ── Proxmox-only inline methods ────────────────────────────────────

    async def info(self) -> dict[str, object]:
        self._require_backend("proxmox", "info")
        return _data(await self._http.get(self._root))

    async def config(self) -> dict[str, object]:
        self._require_backend("proxmox", "config")
        return _data(await self._http.get(f"{self._root}/config"))

    async def pending(self) -> dict[str, object]:
        self._require_backend("proxmox", "pending")
        return _data(await self._http.get(f"{self._root}/pending"))

    async def resources(self) -> dict[str, object]:
        self._require_backend("proxmox", "resources")
        return _data(await self._http.get(f"{self._root}/resources"))

    async def ips(self) -> dict[str, object]:
        self._require_backend("proxmox", "ips")
        return _data(await self._http.get(f"{self._root}/ips"))

    async def available_ips(self) -> dict[str, object]:
        self._require_backend("proxmox", "available_ips")
        return _data(await self._http.get(f"{self._root}/available-ips"))

    async def templates(self) -> dict[str, object]:
        self._require_backend("proxmox", "templates")
        return _data(await self._http.get(f"{self._root}/templates"))

    async def locations(self) -> dict[str, object]:
        self._require_backend("proxmox", "locations")
        return _data(await self._http.get(f"{self._root}/locations"))

    async def console(self) -> ConsoleUrl:
        self._require_backend("proxmox", "console")
        payload = await self._http.get(f"{self._root}/console")
        return ConsoleUrl.model_validate(_data(payload))

    async def console_ssh(self, *, password: str) -> SshConsole:
        self._require_backend("proxmox", "console_ssh")
        payload = await self._http.post(
            f"{self._root}/console/ssh", json={"password": password}
        )
        return SshConsole.model_validate(_data(payload))

    async def network_reconfigure(self) -> dict[str, object]:
        self._require_backend("proxmox", "network_reconfigure")
        return _data(await self._http.post(f"{self._root}/network/reconfigure"))

    async def migrate(self, *, target: str) -> AsyncOperation:
        self._require_backend("proxmox", "migrate")
        payload = await self._http.post(f"{self._root}/migrate", json={"target": target})
        return build_async_operation(self._http, self.id, payload)

    # No suspend()/unsuspend(): see :class:`Vps` for the policy note.

    # ── Cloud-only sub-resources ───────────────────────────────────────

    @property
    def images(self) -> AsyncCloudImagesResource:
        self._require_backend("cloud", "images")
        return AsyncCloudImagesResource(self._http, self.id)

    @property
    def rescue(self) -> AsyncCloudRescueResource:
        self._require_backend("cloud", "rescue")
        return AsyncCloudRescueResource(self._http, self.id)

    @property
    def iso(self) -> AsyncCloudIsoResource:
        self._require_backend("cloud", "iso")
        return AsyncCloudIsoResource(self._http, self.id)

    @property
    def rdns(self) -> AsyncCloudRdnsResource:
        self._require_backend("cloud", "rdns")
        return AsyncCloudRdnsResource(self._http)

    @property
    def ssh_keys(self) -> AsyncCloudSshKeysResource:
        self._require_backend("cloud", "ssh_keys")
        return AsyncCloudSshKeysResource(self._http, self.id)

    # ── Cloud-only inline methods ──────────────────────────────────────

    async def vnc(self) -> VncCredentials:
        self._require_backend("cloud", "vnc")
        return VncCredentials.model_validate(_data(await self._http.get(f"{self._root}/vnc")))

    async def vnc_password(self, password: str) -> None:
        self._require_backend("cloud", "vnc_password")
        await self._http.put(f"{self._root}/vnc-password", json={"password": password})

    async def resize(self, *, instance_size: str) -> dict[str, object]:
        self._require_backend("cloud", "resize")
        return _data(
            await self._http.post(f"{self._root}/resize", json={"instance_size": instance_size})
        )

    async def boot_order(self, order: Literal["cda", "dca"]) -> None:
        self._require_backend("cloud", "boot_order")
        if order not in _VALID_BOOT_ORDER:
            raise ValueError(f"order must be one of {sorted(_VALID_BOOT_ORDER)}")
        await self._http.put(f"{self._root}/boot-order", json={"bootorder": order})

    async def ipv6_enable(self) -> None:
        self._require_backend("cloud", "ipv6_enable")
        await self._http.post(f"{self._root}/ipv6")

    # ── guards ─────────────────────────────────────────────────────────

    def _require_backend(self, expected: VpsBackend, op: str) -> None:
        if self.backend != expected:
            other = "cloud" if expected == "proxmox" else "proxmox"
            hint = (
                f"This VPS is on the {self.backend!r} backend; {op!r} is "
                f"only available on {expected!r}."
            )
            if self.backend == other:
                hint += f" See the {self.backend!r} surface for the equivalent."
            raise BackendNotSupported(self.backend, op, hint=hint)

    def __repr__(self) -> str:
        domain = self.service.domain or "(no domain)"
        return f"AsyncVps(id={self.id}, backend={self.backend!r}, domain={domain!r})"


class AsyncVpsResource:
    """Async counterpart to :class:`VpsResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http
        self._cache: dict[int, VpsBackend] = {}

    async def get(self, service_id: int) -> AsyncVps:
        payload = await self._http.get(f"/account/services/{service_id}")
        service = _service_from_payload(payload)
        return AsyncVps(self._http, self, service)

    async def list(self) -> list[AsyncVps]:
        payload = await self._http.get("/account/services")
        services = _services_from_payload(payload)
        result: list[AsyncVps] = []
        for service in _filter_vps_services(services):
            self._cache_service(service)
            result.append(AsyncVps(self._http, self, service))
        return result

    async def start(self, service_id: int) -> None:
        await self._dispatch_power(service_id, "start")

    async def shutdown(self, service_id: int) -> None:
        await self._dispatch_power(service_id, "shutdown")

    async def reboot(self, service_id: int) -> None:
        await self._dispatch_power(service_id, "reboot")

    async def stop(self, service_id: int) -> None:
        await self._dispatch_power(service_id, "stop")

    async def set_hostname(self, service_id: int, hostname: str) -> None:
        backend = await self._resolve_backend(service_id)
        await self._http.put(
            f"/vps/{backend}/{service_id}/hostname", json={"hostname": hostname}
        )

    async def set_password(self, service_id: int, password: str) -> None:
        backend = await self._resolve_backend(service_id)
        await self._http.put(
            f"/vps/{backend}/{service_id}/password", json={"password": password}
        )

    async def reinstall(
        self,
        service_id: int,
        *,
        template: str,
        password: str,
        confirm: bool,
    ) -> None:
        backend = await self._resolve_backend(service_id)
        await self._http.post(
            f"/vps/{backend}/{service_id}/reinstall",
            json=_reinstall_body(template, password, confirm=confirm),
        )

    async def _dispatch_power(
        self,
        service_id: int,
        action: Literal["start", "stop", "reboot", "shutdown"],
    ) -> None:
        backend = await self._resolve_backend(service_id)
        await self._http.post(
            f"/vps/{backend}/{service_id}/{_power_path(backend, action)}"
        )

    async def _resolve_backend(self, service_id: int) -> VpsBackend:
        cached = self._cache.get(service_id)
        if cached is not None:
            return cached
        payload = await self._http.get(f"/account/services/{service_id}")
        service = _service_from_payload(payload)
        if service.vps_backend is None:
            raise _not_a_vps(service_id)
        self._cache[service_id] = service.vps_backend
        return service.vps_backend

    def _cache_service(self, service: Service) -> None:
        if service.vps_backend is not None:
            self._cache[service.id] = service.vps_backend
