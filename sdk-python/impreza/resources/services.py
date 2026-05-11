"""Services resource — accessed via ``Client.account.services``.

Sync and async variants live in the same file so the parallel surface
is visible side-by-side. Wraps ``GET /account/services``,
``GET /account/services/{id}``, and ``POST /services/{id}/cancel``.

Phase 1.3 shipped the read path (list / get). Phase 3.7 adds
:meth:`cancel` — the non-backend-specific cancellation request that
mirrors :meth:`Vps.cancel` but works on any service (hosting, email,
domain, etc.).
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from ..models.service import Service

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


_VALID_CANCEL_TYPES = {"Immediate", "End of Billing Period"}


def _extract_services(payload: dict[str, object]) -> list[Service]:
    """Pull the ``data.services`` array out of an API response and validate."""
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    services_raw = data.get("services")
    services = services_raw if isinstance(services_raw, list) else []
    return [Service.model_validate(item) for item in services]


def _extract_service(payload: dict[str, object]) -> Service:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    return Service.model_validate(data)


def _list_params(status: str | None) -> dict[str, object] | None:
    if status is None:
        return None
    return {"status": status}


def _cancel_body(*, type: str, reason: str | None) -> dict[str, object]:
    """Build the request body for ``POST /services/{id}/cancel``.

    ``type`` must be one of ``"Immediate"`` or ``"End of Billing
    Period"`` — the server validates the same set and rejects
    anything else with a 400.
    """
    if type not in _VALID_CANCEL_TYPES:
        raise ValueError(
            f"type must be one of {sorted(_VALID_CANCEL_TYPES)}; got {type!r}",
        )
    body: dict[str, object] = {"type": type}
    if reason is not None:
        body["reason"] = reason
    return body


class ServicesResource:
    """Sync read access to the authenticated client's services."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self, *, status: str | None = None) -> list[Service]:
        """Return every service, optionally filtered by status.

        ``status`` accepts these values: ``Active``, ``Suspended``,
        ``Terminated``, ``Pending``, ``Cancelled``.
        """
        payload = self._http.get("/account/services", params=_list_params(status))
        return _extract_services(payload)

    def get(self, service_id: int) -> Service:
        """Return one service by ID. Raises :class:`ResourceNotFound` on 404."""
        payload = self._http.get(f"/account/services/{service_id}")
        return _extract_service(payload)

    def cancel(
        self,
        service_id: int,
        *,
        type: str,
        reason: str | None = None,
    ) -> None:
        """Submit a cancellation request for any service.

        The server creates an ``AddCancelRequest`` — the actual
        termination happens later when staff approves the request and
        the platform runs the module's terminate routine. Customers never
        terminate services directly; this is the only customer-facing
        path to wind a service down.

        ``type`` is one of ``"Immediate"`` (terminate now, prepaid time
        is forfeit) or ``"End of Billing Period"`` (keep the service
        until the next due date — preferred so prepaid days aren't
        thrown away accidentally).
        """
        self._http.post(
            f"/services/{service_id}/cancel",
            json=_cancel_body(type=type, reason=reason),
        )


class AsyncServicesResource:
    """Async read access to the authenticated client's services."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self, *, status: str | None = None) -> list[Service]:
        payload = await self._http.get("/account/services", params=_list_params(status))
        return _extract_services(payload)

    async def get(self, service_id: int) -> Service:
        payload = await self._http.get(f"/account/services/{service_id}")
        return _extract_service(payload)

    async def cancel(
        self,
        service_id: int,
        *,
        type: str,
        reason: str | None = None,
    ) -> None:
        """Async counterpart of :meth:`ServicesResource.cancel`."""
        await self._http.post(
            f"/services/{service_id}/cancel",
            json=_cancel_body(type=type, reason=reason),
        )
