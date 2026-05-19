"""Internal async HTTP transport.

Mirrors :mod:`impreza._http` but built on ``httpx.AsyncClient``. The
public ``AsyncClient`` wraps a single :class:`AsyncHttpClient` instance
and async resources delegate all network work here.

The error mapping, envelope unwrapping, and exponential-backoff logic
are intentionally duplicated rather than abstracted — sync vs async
control flow differs enough (``time.sleep`` vs ``await asyncio.sleep``,
context-manager vs async-context-manager) that a shared base would add
more complexity than it saves.
"""

from __future__ import annotations

import asyncio
import random
from types import TracebackType
from typing import Any

import httpx

from ._http import (
    DEFAULT_BASE_URL,
    DEFAULT_MAX_RETRIES,
    DEFAULT_TIMEOUT,
    USER_AGENT,
)
from .exceptions import (
    ApiError,
    AuthError,
    InsufficientCredit,
    InvalidRequest,
    IpNotWhitelisted,
    NetworkError,
    PermissionDenied,
    RateLimitExceeded,
    ResourceNotFound,
    ServerError,
    UpstreamError,
)


class AsyncHttpClient:
    """Async HTTP transport with auth, retry, and envelope handling."""

    def __init__(
        self,
        api_key: str,
        api_secret: str,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = DEFAULT_TIMEOUT,
        max_retries: int = DEFAULT_MAX_RETRIES,
        proxy: str | None = None,
    ) -> None:
        self._max_retries = max_retries
        self._client = httpx.AsyncClient(
            base_url=base_url.rstrip("/"),
            timeout=timeout,
            headers={
                "X-API-Key": api_key,
                "X-API-Secret": api_secret,
                "Accept": "application/json",
                "User-Agent": USER_AGENT,
            },
            proxy=proxy,
        )

    async def aclose(self) -> None:
        await self._client.aclose()

    async def __aenter__(self) -> AsyncHttpClient:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        await self.aclose()

    # ── public methods ─────────────────────────────────────────────────

    async def get(
        self,
        path: str,
        *,
        params: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return await self._request("GET", path, params=params)

    async def post(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        return await self._request("POST", path, json=json, headers=headers)

    async def patch(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return await self._request("PATCH", path, json=json)

    async def put(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return await self._request("PUT", path, json=json)

    async def delete(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return await self._request("DELETE", path, json=json)

    # ── internals ──────────────────────────────────────────────────────

    async def _request(
        self,
        method: str,
        path: str,
        *,
        params: dict[str, Any] | None = None,
        json: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        attempt = 0
        last_network_error: httpx.RequestError | None = None

        while attempt <= self._max_retries:
            try:
                response = await self._client.request(
                    method, path, params=params, json=json, headers=headers
                )
            except httpx.RequestError as exc:
                last_network_error = exc
                if attempt >= self._max_retries:
                    raise NetworkError(
                        f"Could not reach the Impreza API: {exc}",
                    ) from exc
                await self._sleep_backoff(attempt)
                attempt += 1
                continue

            if response.status_code < 400:
                return self._unwrap(response)

            if response.status_code == 429 and attempt < self._max_retries:
                retry_after = self._parse_retry_after(response)
                wait = retry_after if retry_after is not None else self._backoff(attempt)
                await asyncio.sleep(wait)
                attempt += 1
                continue

            if 500 <= response.status_code < 600 and attempt < self._max_retries:
                await self._sleep_backoff(attempt)
                attempt += 1
                continue

            raise self._exception_from_response(response)

        if last_network_error is not None:
            raise NetworkError(
                f"Could not reach the Impreza API: {last_network_error}",
            ) from last_network_error
        raise NetworkError("Exhausted retries without reaching the API")

    # ── envelope ───────────────────────────────────────────────────────

    def _unwrap(self, response: httpx.Response) -> dict[str, Any]:
        try:
            payload = response.json()
        except ValueError as exc:
            raise ApiError(
                f"Server returned non-JSON response (status {response.status_code})",
                status_code=response.status_code,
            ) from exc

        if not isinstance(payload, dict):
            raise ApiError(
                "Server response was not a JSON object",
                status_code=response.status_code,
            )

        if payload.get("success") is False:
            raise self._exception_from_payload(payload, response.status_code)

        return payload

    # ── retry helpers ──────────────────────────────────────────────────

    def _parse_retry_after(self, response: httpx.Response) -> int | None:
        raw = response.headers.get("Retry-After")
        if raw is None:
            return None
        try:
            return max(0, int(raw))
        except ValueError:
            return None

    def _backoff(self, attempt: int) -> float:
        base = float(2**attempt)
        return base + random.uniform(0, base * 0.5)

    async def _sleep_backoff(self, attempt: int) -> None:
        await asyncio.sleep(self._backoff(attempt))

    # ── exception mapping ──────────────────────────────────────────────

    def _exception_from_response(self, response: httpx.Response) -> ApiError:
        try:
            raw = response.json()
        except ValueError:
            raw = None
        payload: dict[str, Any] = raw if isinstance(raw, dict) else {}
        return self._exception_from_payload(payload, response.status_code, response)

    def _exception_from_payload(
        self,
        payload: dict[str, Any],
        status_code: int,
        response: httpx.Response | None = None,
    ) -> ApiError:
        error_block_raw = payload.get("error")
        error_block: dict[str, Any] = error_block_raw if isinstance(error_block_raw, dict) else {}
        meta_raw = payload.get("meta")
        meta: dict[str, Any] = meta_raw if isinstance(meta_raw, dict) else {}
        details_raw = error_block.get("details")
        details: dict[str, Any] = details_raw if isinstance(details_raw, dict) else {}

        code = error_block.get("code")
        message = error_block.get("message") or f"HTTP {status_code}"
        request_id = meta.get("request_id")

        kwargs: dict[str, Any] = {
            "code": code,
            "request_id": request_id,
            "status_code": status_code,
            "details": details,
        }

        if status_code == 401:
            return AuthError(message, **kwargs)
        if status_code == 403:
            normalized = (code or "").lower() if isinstance(code, str) else ""
            if normalized == "ip_not_whitelisted":
                return IpNotWhitelisted(message, **kwargs)
            return PermissionDenied(message, **kwargs)
        if status_code == 404:
            return ResourceNotFound(message, **kwargs)
        if status_code == 400:
            return InvalidRequest(message, **kwargs)
        if status_code == 402:
            return InsufficientCredit(message, **kwargs)
        if status_code == 429:
            retry_after: int | None = None
            if response is not None:
                retry_after = self._parse_retry_after(response)
            return RateLimitExceeded(message, retry_after=retry_after, **kwargs)
        if status_code in (502, 504):
            return UpstreamError(message, **kwargs)
        if 500 <= status_code < 600:
            return ServerError(message, **kwargs)
        return ApiError(message, **kwargs)
