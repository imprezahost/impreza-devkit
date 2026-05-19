"""Internal HTTP transport.

Not part of the public SDK surface — public consumers should never import
from this module. Resources delegate all network work here. The public
``Client`` wraps a single ``HttpClient`` instance.

Responsibilities:

* Inject ``X-API-Key`` and ``X-API-Secret`` on every request.
* Retry transient failures (5xx and 429) with exponential backoff and jitter.
* Honour the ``Retry-After`` header on 429 responses.
* Unwrap the success envelope (``{"success": true, "data": ..., "meta": ...}``)
  and surface ``meta.request_id`` to callers via exceptions.
* Map status codes and ``error.code`` to typed exceptions from
  :mod:`impreza.exceptions`.
"""

from __future__ import annotations

import random
import time
from types import TracebackType
from typing import Any

import httpx

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

DEFAULT_BASE_URL = "https://api.imprezahost.com/v1"
DEFAULT_TIMEOUT = 30.0
DEFAULT_MAX_RETRIES = 3
USER_AGENT = "impreza-sdk-python/0.1.0a0"


class HttpClient:
    """Sync HTTP transport with auth, retry, and envelope handling."""

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
        self._client = httpx.Client(
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

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> HttpClient:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self.close()

    # ── public methods ─────────────────────────────────────────────────

    def get(
        self,
        path: str,
        *,
        params: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return self._request("GET", path, params=params)

    def post(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        return self._request("POST", path, json=json, headers=headers)

    def patch(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return self._request("PATCH", path, json=json)

    def put(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return self._request("PUT", path, json=json)

    def delete(
        self,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return self._request("DELETE", path, json=json)

    # ── internals ──────────────────────────────────────────────────────

    def _request(
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
                response = self._client.request(
                    method, path, params=params, json=json, headers=headers
                )
            except httpx.RequestError as exc:
                last_network_error = exc
                if attempt >= self._max_retries:
                    raise NetworkError(
                        f"Could not reach the Impreza API: {exc}",
                    ) from exc
                self._sleep_backoff(attempt)
                attempt += 1
                continue

            if response.status_code < 400:
                return self._unwrap(response)

            if response.status_code == 429 and attempt < self._max_retries:
                retry_after = self._parse_retry_after(response)
                wait = retry_after if retry_after is not None else self._backoff(attempt)
                time.sleep(wait)
                attempt += 1
                continue

            if 500 <= response.status_code < 600 and attempt < self._max_retries:
                self._sleep_backoff(attempt)
                attempt += 1
                continue

            raise self._exception_from_response(response)

        # Defensive: loop exited without return / raise. Should be unreachable
        # in normal operation — kept so type checkers see all paths terminated.
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

        # 2xx with success=False shouldn't happen, but if it does, treat as error.
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
        # Exponential with jitter: ~1s, ~2s, ~4s, ~8s ...
        base = float(2**attempt)
        return base + random.uniform(0, base * 0.5)

    def _sleep_backoff(self, attempt: int) -> None:
        time.sleep(self._backoff(attempt))

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
