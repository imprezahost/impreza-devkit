"""``impreza service cancel`` — Phase 3.7.

The non-backend-specific cancellation surface. Mirrors the
:func:`commands.vps.cancel` from 3.3 in shape and policy, but works
on any service id (hosting, email, domain, etc.) — not just VPSs.

Service cancellation is staff-owned by design: the customer submits an
``AddCancelRequest`` via this endpoint, and staff approves the
actual termination later. There is **no** customer-facing path to
terminate a service immediately on the same call; the SDK's
``c.account.services.cancel()`` reflects that.

For VPS-specific cancel (which routes through the same
``AddCancelRequest`` on the server but goes via the bound model),
use ``impreza vps cancel``.
"""

from __future__ import annotations

import typer
from impreza.exceptions import ApiError

from ..output import error, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context
from ._helpers import exit_on_api_error

app = typer.Typer(
    name="service",
    help="Submit cancellation requests for non-VPS services.",
    no_args_is_help=True,
)


_CANCEL_TYPES = {"Immediate", "End of Billing Period"}


@app.command("cancel")
def cancel(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
    cancel_type: str = typer.Option(
        "End of Billing Period",
        "--type", "-t",
        help=(
            "'Immediate' (terminate now, lose prepaid time) or "
            "'End of Billing Period' (keep until next due date). "
            "Default: 'End of Billing Period' so you don't "
            "accidentally throw away prepaid days."
        ),
    ),
    reason: str | None = typer.Option(
        None, "--reason", "-r",
        help="Optional cancellation reason for billing.",
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y",
        help="Skip the service-termination confirmation prompt.",
    ),
) -> None:
    """Submit a cancellation request for a service. **Staff approves**
    the actual termination — this verb only opens the request.

    Wraps ``c.account.services.cancel(id, type=..., reason=...)``.
    Works on any service the authenticated client owns. For VPS-
    specific cancellation, prefer ``impreza vps cancel`` (which
    routes through the same ``AddCancelRequest`` on the server but
    goes via the bound model for backend-specific error messages).
    """
    if cancel_type not in _CANCEL_TYPES:
        error(
            f"--type must be one of {sorted(_CANCEL_TYPES)!r}, "
            f"got: {cancel_type!r}"
        )
        raise typer.Exit(code=1)

    state = from_typer_context(typer_ctx)
    blast = (
        "immediately terminates the service (prepaid time is forfeit) "
        "once staff approves the request"
        if cancel_type == "Immediate"
        else "schedules termination at the end of the current billing "
        "period once staff approves the request"
    )
    confirm_or_exit(
        f"Cancelling service {service_id} ({cancel_type!r}) {blast}.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        try:
            client.account.services.cancel(
                service_id, type=cancel_type, reason=reason
            )
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    success(
        f"Cancellation submitted for service {service_id} ({cancel_type})."
    )
