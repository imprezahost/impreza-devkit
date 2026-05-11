"""Action polling — Phase 1.5.

Wraps the Proxmox async operation queue (``GET /vps/proxmox/{id}/operations/{uuid}``)
into a Future-style object so callers can write::

    op = vps.snapshots.rollback("pre-update")
    op.wait(timeout=600)
    print(op.status)         # "completed" / "failed"

Or in the async client::

    op = await vps.snapshots.rollback("pre-update")
    await op.wait(timeout=600)

The model snapshot is updated in-place on every refresh, so the public
attributes (``status``, ``progress``, ``error``, ``finished_at``) always
reflect the last fetch.

A failed terminal state raises :class:`~impreza.exceptions.OperationFailed`
out of ``.wait()``. A timeout raises :class:`~impreza.exceptions.OperationTimeout`.
For silent / manual handling, use ``op.is_done()`` / ``op.is_success()``
/ ``op.is_failure()`` after each ``op.refresh()`` instead.
"""

from __future__ import annotations

import asyncio
import time
from typing import TYPE_CHECKING

from .exceptions import ApiError, OperationFailed, OperationTimeout
from .models.vps_extras import VpsOperation

if TYPE_CHECKING:  # pragma: no cover
    from ._http import HttpClient
    from ._http_async import AsyncHttpClient


_TERMINAL_SUCCESS = {"completed", "complete", "success", "succeeded", "done"}
_TERMINAL_FAILURE = {"failed", "failure", "cancelled", "canceled", "error"}

DEFAULT_POLL_INTERVAL_SECONDS = 2.0
DEFAULT_TIMEOUT_SECONDS = 600.0


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _operation_from_payload(payload: dict[str, object]) -> VpsOperation:
    """Build a :class:`VpsOperation` from a server response.

    Raises :class:`ApiError` if the payload doesn't carry the queue
    identifiers the polling layer needs (``uuid`` + ``status``) — this
    happens when an endpoint marketed as async actually completed
    synchronously upstream.
    """
    data = _data(payload)
    uuid = data.get("uuid")
    status = data.get("status")
    if not isinstance(uuid, str) or not uuid:
        raise ApiError(
            "Expected an async operation response (with a `uuid` field), "
            f"got keys: {sorted(data.keys())}. The server may have completed "
            "this operation synchronously.",
        )
    if not isinstance(status, str):
        raise ApiError(
            "Expected `status` to be a string in operation response, "
            f"got: {type(status).__name__}.",
        )
    return VpsOperation.model_validate(data)


class _OperationBase:
    """Shared state + status helpers for sync and async futures.

    The sync and async variants only differ in their ``refresh`` and
    ``wait`` IO; everything else (status comparison, attribute proxy,
    repr) lives here.
    """

    __slots__ = ("_service_id", "_snapshot")

    def __init__(self, service_id: int, snapshot: VpsOperation) -> None:
        self._service_id = service_id
        self._snapshot = snapshot

    # ── identity ───────────────────────────────────────────────────────

    @property
    def service_id(self) -> int:
        return self._service_id

    @property
    def uuid(self) -> str:
        return self._snapshot.uuid

    # ── live state (last fetched) ──────────────────────────────────────

    @property
    def status(self) -> str:
        return self._snapshot.status

    @property
    def progress(self) -> float | int | None:
        return self._snapshot.progress

    @property
    def started_at(self) -> str | None:
        return self._snapshot.started_at

    @property
    def finished_at(self) -> str | None:
        return self._snapshot.finished_at

    @property
    def error(self) -> str | None:
        return self._snapshot.error

    @property
    def snapshot(self) -> VpsOperation:
        """Return the underlying :class:`VpsOperation` (last refresh)."""
        return self._snapshot

    # ── status predicates ──────────────────────────────────────────────

    def is_done(self) -> bool:
        normalized = self.status.lower()
        return normalized in _TERMINAL_SUCCESS or normalized in _TERMINAL_FAILURE

    def is_success(self) -> bool:
        return self.status.lower() in _TERMINAL_SUCCESS

    def is_failure(self) -> bool:
        return self.status.lower() in _TERMINAL_FAILURE

    # ── repr ───────────────────────────────────────────────────────────

    def __repr__(self) -> str:
        cls = type(self).__name__
        return (
            f"{cls}(uuid={self.uuid!r}, status={self.status!r}, "
            f"service_id={self.service_id})"
        )


class Operation(_OperationBase):
    """Sync future wrapping a Proxmox async operation."""

    __slots__ = ("_http",)

    def __init__(
        self,
        http: HttpClient,
        service_id: int,
        snapshot: VpsOperation,
    ) -> None:
        super().__init__(service_id, snapshot)
        self._http = http

    def refresh(self) -> Operation:
        """Re-fetch the operation state from the server. Returns ``self``
        for chaining."""
        payload = self._http.get(
            f"/vps/proxmox/{self._service_id}/operations/{self.uuid}"
        )
        self._snapshot = VpsOperation.model_validate(_data(payload))
        return self

    def wait(
        self,
        *,
        timeout: float | None = DEFAULT_TIMEOUT_SECONDS,
        poll_interval: float = DEFAULT_POLL_INTERVAL_SECONDS,
    ) -> Operation:
        """Block until the operation reaches a terminal state.

        Args:
            timeout: maximum wall-clock seconds to block. ``None`` means
                wait indefinitely. Default: 10 minutes.
            poll_interval: seconds between ``GET .../operations/{uuid}``
                calls. Default: 2 seconds.

        Returns ``self`` on success.

        Raises:
            OperationTimeout: when ``timeout`` elapses without a terminal
                state. ``op.refresh()`` can be called manually afterwards
                to keep polling.
            OperationFailed: when the operation finishes in
                ``failed`` / ``cancelled`` / ``error`` state.
        """
        if poll_interval <= 0:
            raise ValueError("poll_interval must be positive")

        deadline = time.monotonic() + timeout if timeout is not None else None

        while not self.is_done():
            if deadline is not None and time.monotonic() >= deadline:
                raise OperationTimeout(self, timeout or 0.0)
            time.sleep(poll_interval)
            self.refresh()

        if self.is_failure():
            raise OperationFailed(self)
        return self


class AsyncOperation(_OperationBase):
    """Async counterpart to :class:`Operation`."""

    __slots__ = ("_http",)

    def __init__(
        self,
        http: AsyncHttpClient,
        service_id: int,
        snapshot: VpsOperation,
    ) -> None:
        super().__init__(service_id, snapshot)
        self._http = http

    async def refresh(self) -> AsyncOperation:
        payload = await self._http.get(
            f"/vps/proxmox/{self._service_id}/operations/{self.uuid}"
        )
        self._snapshot = VpsOperation.model_validate(_data(payload))
        return self

    async def wait(
        self,
        *,
        timeout: float | None = DEFAULT_TIMEOUT_SECONDS,
        poll_interval: float = DEFAULT_POLL_INTERVAL_SECONDS,
    ) -> AsyncOperation:
        if poll_interval <= 0:
            raise ValueError("poll_interval must be positive")

        deadline = time.monotonic() + timeout if timeout is not None else None

        while not self.is_done():
            if deadline is not None and time.monotonic() >= deadline:
                raise OperationTimeout(self, timeout or 0.0)
            await asyncio.sleep(poll_interval)
            await self.refresh()

        if self.is_failure():
            raise OperationFailed(self)
        return self


# ── public factory helpers ─────────────────────────────────────────────


def build_operation(
    http: HttpClient,
    service_id: int,
    payload: dict[str, object],
) -> Operation:
    """Build an :class:`Operation` from a server response payload."""
    return Operation(http, service_id, _operation_from_payload(payload))


def build_async_operation(
    http: AsyncHttpClient,
    service_id: int,
    payload: dict[str, object],
) -> AsyncOperation:
    """Build an :class:`AsyncOperation` from a server response payload."""
    return AsyncOperation(http, service_id, _operation_from_payload(payload))


__all__ = [
    "AsyncOperation",
    "Operation",
    "build_async_operation",
    "build_operation",
]
