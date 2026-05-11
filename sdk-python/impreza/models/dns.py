"""DNS record model.

Used by ``c.domains.dns.list``, ``c.domains.dns.add``, etc.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict


class DnsRecord(BaseModel):
    """A single DNS record on a managed domain.

    ``priority`` is only meaningful for ``MX`` and some ``SRV`` records.
    """

    model_config = ConfigDict(extra="ignore")

    type: str
    host: str
    value: str
    ttl: int | None = None
    priority: int | None = None
