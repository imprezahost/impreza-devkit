"""Email resource — accessed via ``Client.email`` and ``AsyncClient.email``.

Phase 1.4c wires up the two managed-email surfaces our API exposes:

* ``c.email.titan`` — Titan Email (powered by ResellerClub). Three
  read endpoints: order details, required DNS records, single-sign-on
  link to the Titan management panel.
* ``c.email.google`` — Google Workspace (also resold via ResellerClub).
  Two read endpoints (order details, DNS records) plus one write
  (``setup_admin``) that creates the initial admin user during
  provisioning.

Most upstream payloads are forwarded verbatim and resource methods
return ``dict[str, object]`` — the variability between Titan plans
and Workspace tiers makes a tight model worse than a passthrough at
this layer. The SSO endpoint has a stable shape, so it's modeled
(:class:`~impreza.models.email.TitanSsoUrl`).
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from ..models.email import TitanSsoUrl

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


# ── extractors / body builders (shared) ────────────────────────────────


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _extract_dns_records(payload: dict[str, object]) -> list[dict[str, object]]:
    """Extract a list of DNS-record dicts from a ``{dns_records: [...]}`` envelope.

    Falls back to an empty list if the upstream returned something else
    (raw object, missing key) — avoids crashing the SDK on unexpected
    shapes.
    """
    data = _data(payload)
    raw = data.get("dns_records")
    if not isinstance(raw, list):
        return []
    return [item for item in raw if isinstance(item, dict)]


def _setup_admin_body(
    *,
    email_address: str,
    first_name: str,
    last_name: str,
    alternate_email: str,
    name: str,
    company: str,
    zip: str,
) -> dict[str, object]:
    return {
        "email_address": email_address,
        "first_name": first_name,
        "last_name": last_name,
        "alternate_email": alternate_email,
        "name": name,
        "company": company,
        "zip": zip,
    }


# ── sync ───────────────────────────────────────────────────────────────


class TitanResource:
    """Sync Titan Email operations under ``c.email.titan``."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def get(self, domain: str) -> dict[str, object]:
        """Return Titan order details (accounts used/available, plan, expiry)."""
        return _data(self._http.get(f"/email/titan/{domain}"))

    def dns_records(self, domain: str) -> list[dict[str, object]]:
        """List the MX / SPF / DKIM / autodiscover records the domain needs."""
        return _extract_dns_records(self._http.get(f"/email/titan/{domain}/dns"))

    def sso(self, domain: str) -> TitanSsoUrl:
        """Return a single-use SSO link into the Titan admin panel.

        The link is typically valid for 48 hours.
        """
        return TitanSsoUrl.model_validate(_data(self._http.get(f"/email/titan/{domain}/sso")))


class GoogleWorkspaceResource:
    """Sync Google Workspace operations under ``c.email.google``."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def get(self, domain: str) -> dict[str, object]:
        """Return Workspace order details (seats used/available, plan, expiry)."""
        return _data(self._http.get(f"/email/google/{domain}"))

    def dns_records(self) -> list[dict[str, object]]:
        """Return the MX records every Workspace domain needs.

        Account-level — the records are the same across every Workspace
        domain, so the API path is ``/email/google/dns`` (no domain segment).
        """
        return _extract_dns_records(self._http.get("/email/google/dns"))

    def setup_admin(
        self,
        domain: str,
        *,
        email_address: str,
        first_name: str,
        last_name: str,
        alternate_email: str,
        name: str,
        company: str,
        zip: str,
    ) -> dict[str, object]:
        """Create the initial admin user for a fresh Workspace order.

        All fields are required by the registrar. ``zip`` is the postal
        code used for the admin's billing address.
        """
        return _data(
            self._http.post(
                f"/email/google/{domain}/admin",
                json=_setup_admin_body(
                    email_address=email_address,
                    first_name=first_name,
                    last_name=last_name,
                    alternate_email=alternate_email,
                    name=name,
                    company=company,
                    zip=zip,
                ),
            )
        )


class EmailResource:
    """Sync entry point for managed-email services.

    Holds two sub-resources (``c.email.titan`` and ``c.email.google``).
    The split mirrors the API surface — Titan and Google Workspace are
    independent products with their own lifecycles and DNS conventions.
    """

    def __init__(self, http: HttpClient) -> None:
        self.titan = TitanResource(http)
        self.google = GoogleWorkspaceResource(http)


# ── async ──────────────────────────────────────────────────────────────


class AsyncTitanResource:
    """Async counterpart to :class:`TitanResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def get(self, domain: str) -> dict[str, object]:
        return _data(await self._http.get(f"/email/titan/{domain}"))

    async def dns_records(self, domain: str) -> list[dict[str, object]]:
        return _extract_dns_records(await self._http.get(f"/email/titan/{domain}/dns"))

    async def sso(self, domain: str) -> TitanSsoUrl:
        payload = await self._http.get(f"/email/titan/{domain}/sso")
        return TitanSsoUrl.model_validate(_data(payload))


class AsyncGoogleWorkspaceResource:
    """Async counterpart to :class:`GoogleWorkspaceResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def get(self, domain: str) -> dict[str, object]:
        return _data(await self._http.get(f"/email/google/{domain}"))

    async def dns_records(self) -> list[dict[str, object]]:
        return _extract_dns_records(await self._http.get("/email/google/dns"))

    async def setup_admin(
        self,
        domain: str,
        *,
        email_address: str,
        first_name: str,
        last_name: str,
        alternate_email: str,
        name: str,
        company: str,
        zip: str,
    ) -> dict[str, object]:
        return _data(
            await self._http.post(
                f"/email/google/{domain}/admin",
                json=_setup_admin_body(
                    email_address=email_address,
                    first_name=first_name,
                    last_name=last_name,
                    alternate_email=alternate_email,
                    name=name,
                    company=company,
                    zip=zip,
                ),
            )
        )


class AsyncEmailResource:
    """Async counterpart to :class:`EmailResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self.titan = AsyncTitanResource(http)
        self.google = AsyncGoogleWorkspaceResource(http)
