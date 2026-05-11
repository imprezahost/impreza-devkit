"""Cloud-only VPS sub-resources (Phase 1.4b-ii).

Mounted on the :class:`~impreza.resources.vps.Vps` bound model as
``vps.images``, ``vps.rescue``, ``vps.iso``, ``vps.rdns``, and
``vps.ssh_keys`` properties. Each sub-resource raises
:class:`~impreza.exceptions.BackendNotSupported` when accessed on a
Proxmox VPS.

Note on rDNS: the underlying API (``/vps/cloud/rdns/{ip}``) is
account-level (not bound to a particular VM), but the SDK exposes it
through ``vps.rdns`` so users with a :class:`Vps` in hand have direct
access. The ``ip`` argument selects which IP record is being managed
within the account.

Note on SSH keys: ``ssh_keys.list()`` returns account-level keys,
shared across every Cloud VPS the client owns. ``assign(key_ids)``
attaches one or more keys to the current bound VPS.
"""

from __future__ import annotations

import builtins
from typing import TYPE_CHECKING

from ..models.vps_extras import Image, SshKey

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient

# `CloudSshKeysResource` and `AsyncCloudSshKeysResource` define a `list()`
# method, which shadows the builtin `list` within the class body for
# mypy's class-scope name resolution. Use `builtins.list[X]` inside
# those classes for parameter annotations to disambiguate.


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


def _extract_images(payload: dict[str, object]) -> list[Image]:
    return [Image.model_validate(item) for item in _items(payload, "images")]


def _extract_image(payload: dict[str, object]) -> Image:
    return Image.model_validate(_data(payload))


def _extract_ssh_keys(payload: dict[str, object]) -> list[SshKey]:
    return [SshKey.model_validate(item) for item in _items(payload, "keys")]


def _enable_rescue_body(password: str) -> dict[str, object]:
    return {"password": password}


def _mount_iso_body(iso: str) -> dict[str, object]:
    return {"iso": iso}


def _set_rdns_body(hostname: str) -> dict[str, object]:
    return {"hostname": hostname}


def _assign_ssh_keys_body(ssh_keys: list[str | int]) -> dict[str, object]:
    return {"ssh_keys": list(ssh_keys)}


def _resize_body(instance_size: str) -> dict[str, object]:
    return {"instance_size": instance_size}


def _vnc_password_body(password: str) -> dict[str, object]:
    return {"password": password}


def _boot_order_body(order: str) -> dict[str, object]:
    return {"bootorder": order}


# ── sync ───────────────────────────────────────────────────────────────


class CloudImagesResource:
    """Saved images for the Cloud account — ``vps.images``.

    Note: the Cloud image catalog is *account-level*, not per-VM.
    :meth:`list` returns every image the account has saved, regardless of
    which VPS it came from. Each :class:`~impreza.models.vps_extras.Image`
    has a ``vm_id`` field if you need to filter to a specific VPS.

    :meth:`create` snapshots the bound VM's current state into a new image.
    :meth:`restore` restores a chosen image into the bound VM. :meth:`delete`
    removes an image from the account regardless of which VM it came from.
    """

    def __init__(self, http: HttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    @property
    def _vm_root(self) -> str:
        return f"/vps/cloud/{self._vid}/images"

    def list(self) -> list[Image]:
        """Return every image saved on the account (not filtered by VM)."""
        return _extract_images(self._http.get("/vps/cloud/images"))

    def create(self) -> Image:
        """Snapshot the bound VM's current state into a saved image."""
        return _extract_image(self._http.post(self._vm_root))

    def restore(self, image_id: str | int) -> dict[str, object]:
        """Restore an image into the bound VM."""
        return _data(self._http.post(f"{self._vm_root}/{image_id}/restore"))

    def delete(self, image_id: str | int) -> None:
        """Delete an image from the account.

        The API path is ``/vps/cloud/images/{id}`` — account-scoped, no
        ``vmId`` segment. Any image the account owns can be deleted from
        any bound VPS.
        """
        self._http.delete(f"/vps/cloud/images/{image_id}")


class CloudRescueResource:
    """Rescue mode for a Cloud VPS — ``vps.rescue``."""

    def __init__(self, http: HttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    @property
    def _root(self) -> str:
        return f"/vps/cloud/{self._vid}/rescue"

    def enable(self, *, password: str) -> dict[str, object]:
        """Enable rescue mode. Reboot the VM to enter rescue."""
        return _data(self._http.post(self._root, json=_enable_rescue_body(password)))

    def disable(self) -> None:
        self._http.delete(self._root)


class CloudIsoResource:
    """ISO mounting on a Cloud VPS — ``vps.iso``."""

    def __init__(self, http: HttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    @property
    def _base(self) -> str:
        return f"/vps/cloud/{self._vid}/iso"

    def mount(self, iso: str) -> dict[str, object]:
        return _data(self._http.post(f"{self._base}/mount", json=_mount_iso_body(iso)))

    def unmount(self) -> None:
        self._http.delete(self._base)


class CloudRdnsResource:
    """Reverse-DNS management — ``vps.rdns``.

    Account-scoped at the API layer (``/vps/cloud/rdns/{ip}``) but
    surfaced through the bound VPS for ergonomic access.
    """

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def get(self, ip: str) -> dict[str, object]:
        return _data(self._http.get(f"/vps/cloud/rdns/{ip}"))

    def set(self, ip: str, hostname: str) -> dict[str, object]:
        return _data(self._http.put(f"/vps/cloud/rdns/{ip}", json=_set_rdns_body(hostname)))

    def delete(self, ip: str) -> None:
        self._http.delete(f"/vps/cloud/rdns/{ip}")


class CloudSshKeysResource:
    """SSH key management — ``vps.ssh_keys``.

    ``list()`` returns account-level keys (shared across every Cloud VPS).
    ``assign()`` attaches one or more keys to the current bound VPS.
    """

    def __init__(self, http: HttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    def list(self) -> list[SshKey]:
        """Return every SSH key registered on this Cloud account."""
        return _extract_ssh_keys(self._http.get("/vps/cloud/ssh-keys"))

    def assign(self, ssh_keys: builtins.list[str | int]) -> dict[str, object]:
        """Assign one or more existing account-level keys to this VPS."""
        return _data(
            self._http.post(
                f"/vps/cloud/{self._vid}/ssh-keys",
                json=_assign_ssh_keys_body(ssh_keys),
            )
        )


# ── async ──────────────────────────────────────────────────────────────


class AsyncCloudImagesResource:
    """Async counterpart to :class:`CloudImagesResource`.

    See that class's docstring for the account-level :meth:`list` rationale.
    """

    def __init__(self, http: AsyncHttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    @property
    def _vm_root(self) -> str:
        return f"/vps/cloud/{self._vid}/images"

    async def list(self) -> list[Image]:
        return _extract_images(await self._http.get("/vps/cloud/images"))

    async def create(self) -> Image:
        return _extract_image(await self._http.post(self._vm_root))

    async def restore(self, image_id: str | int) -> dict[str, object]:
        return _data(await self._http.post(f"{self._vm_root}/{image_id}/restore"))

    async def delete(self, image_id: str | int) -> None:
        await self._http.delete(f"/vps/cloud/images/{image_id}")


class AsyncCloudRescueResource:
    def __init__(self, http: AsyncHttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    @property
    def _root(self) -> str:
        return f"/vps/cloud/{self._vid}/rescue"

    async def enable(self, *, password: str) -> dict[str, object]:
        return _data(await self._http.post(self._root, json=_enable_rescue_body(password)))

    async def disable(self) -> None:
        await self._http.delete(self._root)


class AsyncCloudIsoResource:
    def __init__(self, http: AsyncHttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    @property
    def _base(self) -> str:
        return f"/vps/cloud/{self._vid}/iso"

    async def mount(self, iso: str) -> dict[str, object]:
        return _data(await self._http.post(f"{self._base}/mount", json=_mount_iso_body(iso)))

    async def unmount(self) -> None:
        await self._http.delete(self._base)


class AsyncCloudRdnsResource:
    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def get(self, ip: str) -> dict[str, object]:
        return _data(await self._http.get(f"/vps/cloud/rdns/{ip}"))

    async def set(self, ip: str, hostname: str) -> dict[str, object]:
        return _data(
            await self._http.put(f"/vps/cloud/rdns/{ip}", json=_set_rdns_body(hostname))
        )

    async def delete(self, ip: str) -> None:
        await self._http.delete(f"/vps/cloud/rdns/{ip}")


class AsyncCloudSshKeysResource:
    def __init__(self, http: AsyncHttpClient, vm_id: int) -> None:
        self._http = http
        self._vid = vm_id

    async def list(self) -> list[SshKey]:
        return _extract_ssh_keys(await self._http.get("/vps/cloud/ssh-keys"))

    async def assign(self, ssh_keys: builtins.list[str | int]) -> dict[str, object]:
        return _data(
            await self._http.post(
                f"/vps/cloud/{self._vid}/ssh-keys",
                json=_assign_ssh_keys_body(ssh_keys),
            )
        )
