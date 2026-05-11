"""Email-service response models (Phase 1.4c).

Most email-service payloads come from upstream registrars (Titan,
Google Workspace) whose shapes vary across product tiers and are
forwarded by the API verbatim. Resources return ``dict[str, object]``
for those. Only the well-defined shapes get a model here.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict


class TitanSsoUrl(BaseModel):
    """Single-sign-on links into the Titan Email management panel.

    Returned by :meth:`impreza.resources.email.TitanResource.sso`.
    The links are typically valid for 48 hours.
    """

    model_config = ConfigDict(extra="ignore")

    sso_url: str
    iframe_url: str | None = None
