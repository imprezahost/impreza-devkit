"""Dedicated server resource — accessed via ``Client.dedicated`` and
``AsyncClient.dedicated``.

Wraps the public ``/dedicated/*`` namespace. Operations are gated by
per-service capabilities: a feature is only available if the service
advertises it in :meth:`DedicatedResource.capabilities`. Calling an
endpoint whose capability is not on that list raises
:class:`~impreza.exceptions.ResourceNotFound` with code ``NOT_SUPPORTED``.

Reinstall is destructive and requires both ``confirm=True`` and the
``X-Impreza-Confirm: WIPE`` header. The resource injects the header
when ``confirm=True`` is passed.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Literal

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


def _data(payload: dict[str, Any]) -> Any:
    return payload.get("data")


def _reinstall_body(
    *,
    os_id: str,
    password: str,
    os_label: str | None,
    confirm: bool,
) -> dict[str, Any]:
    if not confirm:
        raise ValueError(
            "reinstall wipes all data — pass confirm=True and the SDK will also "
            "send the required X-Impreza-Confirm: WIPE header for you.",
        )
    body: dict[str, Any] = {"os_id": os_id, "password": password, "confirm": True}
    if os_label is not None:
        body["os_label"] = os_label
    return body


_PowerAction = Literal["start", "shutdown", "reboot"]
_FirewallState = Literal["always_on", "redirect_on_attack"] | None
_FirewallSensitivity = Literal["low", "normal", "medium", "high"] | None
_BandwidthType = Literal[
    "port_bits",
    "port_upkts",
    "port_percent",
    "port_errors",
    "port_pktsize",
    "port_discards",
]
_BandwidthScale = Literal["day", "week", "month"]


# ── sync ───────────────────────────────────────────────────────────────


class DedicatedResource:
    """Sync surface for ``/dedicated/*``.

    All write methods raise on upstream failure — see
    :mod:`impreza.exceptions` for the typed exception hierarchy.
    Operations whose underlying service can't be applied automatically
    return ``{"status": "queued", "message": "..."}`` rather than the
    sync result; callers should treat that as accepted-for-later (an
    operator on our side completes it within a few hours).
    """

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    # ── discovery ──────────────────────────────────────────────────────

    def list(self) -> list[dict[str, Any]]:
        """List every dedicated service the client owns."""
        payload = self._http.get("/dedicated")
        return _data(payload) or []

    def info(self, service_id: int) -> dict[str, Any]:
        """Full details for a service (summary, details, capabilities)."""
        return _data(self._http.get(f"/dedicated/{service_id}")) or {}

    def capabilities(self, service_id: int) -> dict[str, Any]:
        """Capability strings advertised by the service. Call this before
        any capability-gated method (firewall / bandwidth / vpn / kvm /
        reinstall / power.*) to know what's available."""
        return _data(self._http.get(f"/dedicated/{service_id}/capabilities")) or {}

    def status(self, service_id: int) -> dict[str, Any]:
        """Current power / provisioning state."""
        return _data(self._http.get(f"/dedicated/{service_id}/status")) or {}

    def ips(self, service_id: int) -> dict[str, Any]:
        """List IPs with current PTR."""
        return _data(self._http.get(f"/dedicated/{service_id}/ips")) or {}

    def os_images(self, service_id: int) -> list[dict[str, Any]]:
        """OS images available for reinstall."""
        result = _data(self._http.get(f"/dedicated/{service_id}/os-images"))
        return result if isinstance(result, list) else []

    # ── power ──────────────────────────────────────────────────────────

    def start(self, service_id: int) -> None:
        """Power on."""
        self._http.post(f"/dedicated/{service_id}/start")

    def shutdown(self, service_id: int) -> None:
        """Graceful shutdown."""
        self._http.post(f"/dedicated/{service_id}/shutdown")

    def reboot(self, service_id: int) -> None:
        """Reboot."""
        self._http.post(f"/dedicated/{service_id}/reboot")

    # ── rDNS ───────────────────────────────────────────────────────────

    def set_rdns(self, service_id: int, ip: str, hostname: str) -> dict[str, Any]:
        """Set PTR for a single IP. Applied synchronously on most services;
        on services without an automated path the response is
        ``{"status": "queued", ...}``."""
        payload = self._http.put(
            f"/dedicated/{service_id}/ips/{ip}/rdns", json={"hostname": hostname}
        )
        return _data(payload) or {}

    def reset_rdns(self, service_id: int) -> dict[str, Any]:
        """Reset PTR to the Impreza default (`<ip-with-dashes>.impreza.host`)
        on every IP."""
        payload = self._http.post(f"/dedicated/{service_id}/ips/rdns/reset")
        return _data(payload) or {}

    # ── reinstall (destructive) ────────────────────────────────────────

    def reinstall(
        self,
        service_id: int,
        *,
        os_id: str,
        password: str,
        confirm: bool,
        os_label: str | None = None,
    ) -> dict[str, Any]:
        """Reinstall the OS. Destructive — wipes everything.

        ``confirm=True`` is mandatory. The SDK automatically sets the
        ``X-Impreza-Confirm: WIPE`` header the API requires alongside the
        body confirmation.

        On services that support a synchronous reinstall path, the result
        includes ``root_password``. Otherwise the response is
        ``{"status": "queued", "message": "..."}`` and our team executes
        the reinstall within a few hours.
        """
        body = _reinstall_body(
            os_id=os_id, password=password, os_label=os_label, confirm=confirm
        )
        payload = self._http.post(
            f"/dedicated/{service_id}/reinstall",
            json=body,
            headers={"X-Impreza-Confirm": "WIPE"},
        )
        return _data(payload) or {}

    # ── KVM / IPMI ─────────────────────────────────────────────────────

    def kvm(self, service_id: int) -> dict[str, Any]:
        """Current KVM / IPMI access info."""
        return _data(self._http.get(f"/dedicated/{service_id}/kvm")) or {}

    def enable_kvm(self, service_id: int) -> dict[str, Any]:
        """Enable KVM session. The server injects the calling client's
        public IP as the session-binding IP automatically for services
        that need it — nothing for the caller to pass."""
        payload = self._http.post(f"/dedicated/{service_id}/kvm/enable")
        return _data(payload) or {}

    def disable_kvm(self, service_id: int) -> dict[str, Any]:
        """Disable an active KVM session."""
        payload = self._http.delete(f"/dedicated/{service_id}/kvm")
        return _data(payload) or {}

    # ── Firewall (requires `firewall` capability) ──────────────────────

    def firewall(self, service_id: int) -> dict[str, Any]:
        """DDoS firewall state. Requires the ``firewall`` capability —
        other services return NOT_SUPPORTED."""
        return _data(self._http.get(f"/dedicated/{service_id}/firewall")) or {}

    def set_firewall(
        self,
        service_id: int,
        *,
        ip: str,
        state: _FirewallState = None,
        sensitivity: _FirewallSensitivity = None,
    ) -> dict[str, Any]:
        """Update firewall state / sensitivity for an IP. Requires the
        ``firewall`` capability.

        Pass ``None`` for ``state`` or ``sensitivity`` to leave unchanged.
        """
        body: dict[str, Any] = {"ip": ip, "state": state, "sensitivity": sensitivity}
        return _data(self._http.put(f"/dedicated/{service_id}/firewall", json=body)) or {}

    def ddos_logs(self, service_id: int) -> dict[str, Any]:
        """DDoS attack logs. Requires the ``firewall`` capability."""
        return _data(self._http.get(f"/dedicated/{service_id}/firewall/logs")) or {}

    # ── Bandwidth (requires `bandwidth` capability) ────────────────────

    def bandwidth(
        self,
        service_id: int,
        *,
        type: _BandwidthType = "port_bits",
        scale: _BandwidthScale = "month",
    ) -> dict[str, Any]:
        """Bandwidth graph (PNG base64). Requires the ``bandwidth`` capability."""
        payload = self._http.get(
            f"/dedicated/{service_id}/bandwidth",
            params={"type": type, "scale": scale},
        )
        return _data(payload) or {}

    # ── VPN (requires `vpn` capability) ────────────────────────────────

    def vpn(self, service_id: int) -> dict[str, Any]:
        """Rotating VPN password + OpenVPN client URL. Requires the
        ``vpn`` capability."""
        return _data(self._http.get(f"/dedicated/{service_id}/vpn")) or {}


# ── async ──────────────────────────────────────────────────────────────


class AsyncDedicatedResource:
    """Async counterpart to :class:`DedicatedResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self) -> list[dict[str, Any]]:
        payload = await self._http.get("/dedicated")
        return _data(payload) or []

    async def info(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}")) or {}

    async def capabilities(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}/capabilities")) or {}

    async def status(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}/status")) or {}

    async def ips(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}/ips")) or {}

    async def os_images(self, service_id: int) -> list[dict[str, Any]]:
        result = _data(await self._http.get(f"/dedicated/{service_id}/os-images"))
        return result if isinstance(result, list) else []

    async def start(self, service_id: int) -> None:
        await self._http.post(f"/dedicated/{service_id}/start")

    async def shutdown(self, service_id: int) -> None:
        await self._http.post(f"/dedicated/{service_id}/shutdown")

    async def reboot(self, service_id: int) -> None:
        await self._http.post(f"/dedicated/{service_id}/reboot")

    async def set_rdns(self, service_id: int, ip: str, hostname: str) -> dict[str, Any]:
        payload = await self._http.put(
            f"/dedicated/{service_id}/ips/{ip}/rdns", json={"hostname": hostname}
        )
        return _data(payload) or {}

    async def reset_rdns(self, service_id: int) -> dict[str, Any]:
        payload = await self._http.post(f"/dedicated/{service_id}/ips/rdns/reset")
        return _data(payload) or {}

    async def reinstall(
        self,
        service_id: int,
        *,
        os_id: str,
        password: str,
        confirm: bool,
        os_label: str | None = None,
    ) -> dict[str, Any]:
        body = _reinstall_body(
            os_id=os_id, password=password, os_label=os_label, confirm=confirm
        )
        payload = await self._http.post(
            f"/dedicated/{service_id}/reinstall",
            json=body,
            headers={"X-Impreza-Confirm": "WIPE"},
        )
        return _data(payload) or {}

    async def kvm(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}/kvm")) or {}

    async def enable_kvm(self, service_id: int) -> dict[str, Any]:
        payload = await self._http.post(f"/dedicated/{service_id}/kvm/enable")
        return _data(payload) or {}

    async def disable_kvm(self, service_id: int) -> dict[str, Any]:
        payload = await self._http.delete(f"/dedicated/{service_id}/kvm")
        return _data(payload) or {}

    async def firewall(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}/firewall")) or {}

    async def set_firewall(
        self,
        service_id: int,
        *,
        ip: str,
        state: _FirewallState = None,
        sensitivity: _FirewallSensitivity = None,
    ) -> dict[str, Any]:
        body: dict[str, Any] = {"ip": ip, "state": state, "sensitivity": sensitivity}
        return _data(await self._http.put(f"/dedicated/{service_id}/firewall", json=body)) or {}

    async def ddos_logs(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}/firewall/logs")) or {}

    async def bandwidth(
        self,
        service_id: int,
        *,
        type: _BandwidthType = "port_bits",
        scale: _BandwidthScale = "month",
    ) -> dict[str, Any]:
        payload = await self._http.get(
            f"/dedicated/{service_id}/bandwidth",
            params={"type": type, "scale": scale},
        )
        return _data(payload) or {}

    async def vpn(self, service_id: int) -> dict[str, Any]:
        return _data(await self._http.get(f"/dedicated/{service_id}/vpn")) or {}
