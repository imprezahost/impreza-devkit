"""Hosting resource — accessed via ``Client.hosting`` and ``AsyncClient.hosting``.

Phase 1.4c wraps the three cPanel/WHM endpoints exposed by the API:

* ``GET /hosting/{serviceId}`` — full account summary (IP, plan, disk
  usage, bandwidth, status). The shape is whatever the upstream WHM
  returned, forwarded verbatim — :meth:`get` returns ``dict[str, object]``.
* ``GET /hosting/{serviceId}/nameservers`` — list of nameserver hostnames
  configured on the cPanel server.
* ``POST /hosting/{serviceId}/autossl`` — kicks off an AutoSSL run
  (Let's Encrypt via cPanel). The cron itself is asynchronous on the
  server side; the call returns immediately with a status payload.

Sync and async share extractor helpers at the top.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


# ── extractors (shared) ────────────────────────────────────────────────


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _extract_nameservers(payload: dict[str, object]) -> list[str]:
    data = _data(payload)
    raw = data.get("nameservers")
    if not isinstance(raw, list):
        return []
    return [str(ns) for ns in raw if isinstance(ns, str)]


# ── sync ───────────────────────────────────────────────────────────────


class HostingResource:
    """Sync access to cPanel/WHM hosting services."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def get(self, service_id: int) -> dict[str, object]:
        """Return the cPanel account summary for the service.

        Shape varies by WHM version and reseller config — common fields
        include ``ip``, ``plan``, ``disk_used``, ``disk_limit``,
        ``bw_used``, ``bw_limit``, ``status``.
        """
        return _data(self._http.get(f"/hosting/{service_id}"))

    def nameservers(self, service_id: int) -> list[str]:
        """Return the nameservers configured on the hosting server."""
        return _extract_nameservers(self._http.get(f"/hosting/{service_id}/nameservers"))

    def trigger_autossl(self, service_id: int) -> dict[str, object]:
        """Trigger an AutoSSL run for the cPanel account.

        Returns the upstream status payload (typically ``{message, details}``).
        AutoSSL itself runs asynchronously on the WHM cron — there is no
        SDK-side polling for completion at this layer.
        """
        return _data(self._http.post(f"/hosting/{service_id}/autossl"))


# ── async ──────────────────────────────────────────────────────────────


class AsyncHostingResource:
    """Async counterpart to :class:`HostingResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def get(self, service_id: int) -> dict[str, object]:
        return _data(await self._http.get(f"/hosting/{service_id}"))

    async def nameservers(self, service_id: int) -> list[str]:
        return _extract_nameservers(
            await self._http.get(f"/hosting/{service_id}/nameservers")
        )

    async def trigger_autossl(self, service_id: int) -> dict[str, object]:
        return _data(await self._http.post(f"/hosting/{service_id}/autossl"))
