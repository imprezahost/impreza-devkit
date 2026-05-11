"""Service-related response models."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict

VpsBackend = Literal["proxmox", "cloud"]


class Service(BaseModel):
    """A service owned by the authenticated client.

    Returned by ``GET /account/services`` and ``GET /account/services/{id}``.
    Covers hosting, VPS, domain, and reseller services. For VPS services,
    the ``vps_backend`` field is the stable discriminator the SDK uses to
    smart-dispatch between ``/vps/proxmox/...`` and ``/vps/cloud/...``
    endpoints.

    Unknown fields are silently ignored (forward-compatible).
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    domain: str | None = None
    status: str
    product: str
    product_group: str | None = None
    billing_cycle: str | None = None
    amount: float | None = None
    dedicated_ip: str | None = None
    registered_at: str | None = None
    next_due: str | None = None
    vps_backend: VpsBackend | None = None
