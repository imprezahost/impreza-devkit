"""Pydantic models for the backend-specific VPS surfaces (Phase 1.4b-ii).

These cover the data returned by Proxmox-only and Cloud-only sub-resources
(snapshots, backups, schedules, operations, images, SSH keys, consoles).
The models use ``extra="ignore"`` so upstream additions land transparently.

Endpoints whose payload shape varies a lot upstream (raw Proxmox config
dumps, raw Cloud info objects, etc.) keep returning ``dict[str, object]``
in the resource layer — only the well-known shapes get a model here.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field

# ── Proxmox-only ───────────────────────────────────────────────────────


class Snapshot(BaseModel):
    """A Proxmox VM snapshot.

    Returned by ``c.vps.get(...).snapshots.list()`` (and async).
    """

    model_config = ConfigDict(extra="ignore")

    name: str
    description: str | None = None
    created_at: str | None = None


class Backup(BaseModel):
    """A Proxmox VM backup."""

    model_config = ConfigDict(extra="ignore")

    id: str | int
    date: str | None = None
    size: int | None = None
    mode: str | None = None
    compress: str | None = None
    protected: bool | None = None
    notes: str | None = None


class BackupSchedule(BaseModel):
    """A scheduled Proxmox backup job."""

    model_config = ConfigDict(extra="ignore")

    id: str | int
    dow: str | None = Field(default=None, description="Day-of-week selector, e.g. 'mon,wed,fri'")
    hour: int | None = None
    minute: int | None = None
    mode: str | None = None
    compress: str | None = None


class VpsOperation(BaseModel):
    """A queued Proxmox operation (reinstall, migrate, etc.).

    Phase 1.5 will add ``op.wait()`` / ``op.refresh()`` polling helpers.
    For now this is a raw read-only view into the queue.
    """

    model_config = ConfigDict(extra="ignore")

    uuid: str
    status: str
    progress: float | int | None = None
    started_at: str | None = None
    finished_at: str | None = None
    error: str | None = None


class ConsoleUrl(BaseModel):
    """noVNC browser-console URL returned by ``vps.console()``.

    Typically valid for ~2h. Open in a browser.
    """

    model_config = ConfigDict(extra="ignore")

    url: str
    expires_at: str | None = None


class SshConsole(BaseModel):
    """WebSocket SSH console credentials returned by ``vps.console_ssh(password=...)``."""

    model_config = ConfigDict(extra="ignore")

    ws_url: str
    encrypted_token: str | None = None
    expires_at: str | None = None


# ── Cloud-only ─────────────────────────────────────────────────────────


class Image(BaseModel):
    """A saved Cloud VM image (snapshot of current state, restorable).

    Returned by ``vps.images.list()`` and ``c.vps.cloud_images.list()``.
    """

    model_config = ConfigDict(extra="ignore")

    id: str | int
    name: str | None = None
    vm_id: str | int | None = None
    size: int | None = None
    created_at: str | None = None
    status: str | None = None


class SshKey(BaseModel):
    """A registered SSH key (account-level on Cloud backend).

    Returned by ``c.vps.cloud_ssh_keys.list()``.
    """

    model_config = ConfigDict(extra="ignore")

    id: str | int
    name: str
    fingerprint: str | None = None


class VncCredentials(BaseModel):
    """Cloud VNC client credentials returned by ``vps.vnc()``.

    Use these with a desktop VNC client (TigerVNC, RealVNC).
    """

    model_config = ConfigDict(extra="ignore")

    ip: str
    port: int
    password: str
