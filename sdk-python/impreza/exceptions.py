"""Exception hierarchy for the Impreza SDK.

Every error raised by the SDK is a subclass of ``ImprezaError``, so
a single ``except ImprezaError`` catches anything the SDK can throw.

Network-level failures (connect / DNS / timeout) raise ``NetworkError``.
HTTP responses with an error envelope raise ``ApiError`` or one of its
status-code-specific subclasses, with ``code``, ``message``,
``request_id``, and ``details`` populated from the JSON body.
"""

from __future__ import annotations

from typing import Any


class ImprezaError(Exception):
    """Base exception for everything raised by the SDK."""

    def __init__(
        self,
        message: str,
        *,
        code: str | None = None,
        request_id: str | None = None,
        status_code: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.code = code
        self.request_id = request_id
        self.status_code = status_code
        self.details = details or {}

    def __str__(self) -> str:
        parts: list[str] = [self.message]
        if self.code:
            parts.append(f"(code={self.code})")
        if self.request_id:
            parts.append(f"[request_id={self.request_id}]")
        return " ".join(parts)


class NetworkError(ImprezaError):
    """The SDK could not reach the API at all (DNS / connect / timeout)."""


class ApiError(ImprezaError):
    """The API returned an error response.

    Catch-all for non-2xx responses that do not match a more specific
    subclass below.
    """


class AuthError(ApiError):
    """401 — invalid or missing API credentials."""


class PermissionDenied(ApiError):
    """403 — caller is authenticated but not allowed to access the resource."""


class IpNotWhitelisted(PermissionDenied):
    """403 with ``code == "IP_NOT_WHITELISTED"`` — the caller's IP is blocked.

    Inherits from ``PermissionDenied`` so existing 403 handlers keep working.
    """


class ResourceNotFound(ApiError):
    """404 — the requested resource does not exist or is not visible to this key."""


class InvalidRequest(ApiError):
    """400 — request shape is invalid (missing/bad fields, etc.)."""


class InsufficientCredit(ApiError):
    """402 — the operation requires more balance than the account has available."""


class RateLimitExceeded(ApiError):
    """429 — too many requests. Wait ``retry_after`` seconds before trying again.

    ``retry_after`` reflects the server's ``Retry-After`` header when present;
    ``None`` means the server did not advertise a wait time.
    """

    def __init__(
        self,
        message: str,
        *,
        retry_after: int | None = None,
        **kwargs: Any,
    ) -> None:
        super().__init__(message, **kwargs)
        self.retry_after = retry_after


class UpstreamError(ApiError):
    """502 / 504 — error from a provider we depend on (registrar, hypervisor, etc.)."""


class ServerError(ApiError):
    """5xx (other) — generic server-side failure that is not specifically classified."""


class OperationTimeout(ImprezaError):
    """Raised by ``Operation.wait()`` when the timeout elapses without
    the upstream operation reaching a terminal state.

    The partially-resolved :class:`Operation` is attached as
    ``.operation`` so callers can inspect the last-known status,
    progress, and error fields, and / or call ``op.refresh()`` to keep
    polling manually.
    """

    def __init__(self, operation: object, timeout: float) -> None:
        op_uuid = getattr(operation, "uuid", "<unknown>")
        op_status = getattr(operation, "status", "<unknown>")
        super().__init__(
            f"Operation {op_uuid!s} did not finish within {timeout:.1f}s "
            f"(last status: {op_status!s}).",
        )
        self.operation = operation
        self.timeout = timeout


class OperationFailed(ImprezaError):
    """Raised by ``Operation.wait()`` when the upstream operation reaches
    a terminal failure state (``failed``, ``cancelled``, ``error``).

    The :class:`Operation` is attached as ``.operation`` so callers can
    read the final status / progress / error message without re-fetching.
    """

    def __init__(self, operation: object) -> None:
        op_uuid = getattr(operation, "uuid", "<unknown>")
        op_status = getattr(operation, "status", "<unknown>")
        op_error = getattr(operation, "error", None)
        suffix = f" — {op_error}" if op_error else ""
        super().__init__(
            f"Operation {op_uuid!s} ended in {op_status!r}{suffix}.",
        )
        self.operation = operation


class TopupTimeout(ImprezaError):
    """Raised by ``TopupInvoice.wait_until_paid()`` when the timeout elapses
    without the invoice reaching a terminal state.

    The partially-resolved :class:`TopupInvoice` is attached as
    ``.invoice`` so callers can inspect the last-known status, payment URL,
    and expiry, and / or call ``invoice.refresh()`` to keep polling
    manually. Crypto confirmations can be slow — a timeout doesn't mean
    the payment will never settle, just that the SDK gave up waiting.
    """

    def __init__(self, invoice: object, timeout: float) -> None:
        invoice_id = getattr(invoice, "invoice_id", "<unknown>")
        invoice_status = getattr(invoice, "status", "<unknown>")
        super().__init__(
            f"Top-up invoice {invoice_id!s} did not reach a terminal "
            f"state within {timeout:.1f}s (last status: {invoice_status!s}).",
        )
        self.invoice = invoice
        self.timeout = timeout


class TopupFailed(ImprezaError):
    """Raised by ``TopupInvoice.wait_until_paid()`` when the invoice reaches
    a terminal failure state (``cancelled``, ``refunded``).

    The :class:`TopupInvoice` is attached as ``.invoice`` so callers can
    read the final status without re-fetching.
    """

    def __init__(self, invoice: object) -> None:
        invoice_id = getattr(invoice, "invoice_id", "<unknown>")
        invoice_status = getattr(invoice, "status", "<unknown>")
        super().__init__(
            f"Top-up invoice {invoice_id!s} ended in {invoice_status!r}.",
        )
        self.invoice = invoice


class WebhookSignatureMismatch(ImprezaError):
    """Raised by :func:`impreza.webhooks.verify_signature` when the supplied
    HMAC signature does not match the body / secret.

    The library never tells you *why* it didn't match (timing-safe by
    design — leaking which half of the comparison failed first would let
    attackers narrow signatures byte-by-byte). The exception message is
    intentionally vague: ``"signature mismatch"``.

    The exception is a subclass of :class:`ImprezaError`, *not*
    :class:`ApiError` — there is no API call associated, this is a
    purely client-side guard you run on incoming webhook deliveries.
    """


class BackendNotSupported(ImprezaError):
    """Operation requested on a VPS backend that does not support it.

    Raised by backend-specific VPS sub-resources when the bound :class:`Vps`
    is on the wrong backend — e.g. accessing ``vps.snapshots`` on a Cloud
    VPS, or ``vps.images`` on a Proxmox VPS. Each operation is exclusive to
    one backend; this exception spells out the mismatch and points at the
    equivalent (or absence thereof) on the other backend.

    Attributes:
        backend: the actual backend of the bound VPS (``"proxmox"`` or
            ``"cloud"``).
        operation: short name of the operation that was not supported.
    """

    def __init__(
        self,
        backend: str,
        operation: str,
        *,
        hint: str | None = None,
    ) -> None:
        message = (
            f"Operation {operation!r} is not supported on the {backend!r} "
            "VPS backend."
        )
        if hint:
            message += f" {hint}"
        super().__init__(message)
        self.backend = backend
        self.operation = operation
        self.hint = hint
