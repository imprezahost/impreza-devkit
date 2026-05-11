"""VPS-related response models.

Phase 1.4b-i ships only the common subset of VPS state shared between
the Proxmox and Cloud backends. Backend-specific shapes (snapshots,
backups, images, rescue mode, ISO mounts) land in 1.4b-ii.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict


class VpsStatus(BaseModel):
    """Power state and runtime metrics common to all VPS backends.

    All fields except ``power_state`` are optional because the Cloud
    backend only exposes power state in its ``GET /vps/cloud/{id}``
    response — CPU, memory, and uptime metrics are Proxmox-only today.

    Returned by :meth:`impreza.resources.vps.Vps.status` (sync) and
    its async counterpart.
    """

    model_config = ConfigDict(extra="ignore")

    power_state: str
    cpu_usage: float | None = None
    memory_used: int | None = None
    memory_total: int | None = None
    uptime: int | None = None
