"""``impreza account`` subcommand surface — Phase 2.2.

Three verbs reading from `c.account.*`:

* ``impreza account info``
    Authenticated client's profile + balance, rendered as a
    field/value table (or JSON / YAML).

* ``impreza account balance``
    Just the numeric balance + currency. Terse, scriptable —
    designed to drop into shell pipelines (``$(impreza account balance
    --raw)`` returns a single number).

* ``impreza account services [--status STATUS]``
    Active / pending / cancelled / etc. services on the account,
    one row per service with id / domain / product / status / next-due
    columns. Optional ``--status`` filter passes through to the SDK.

All three commands route through :func:`impreza_cli.sdk.make_client_or_exit`,
so context resolution failures (no contexts configured, missing
default, unknown ``--context`` override) surface as friendly stderr
errors with a non-zero exit code rather than tracebacks.
"""

from __future__ import annotations

import sys
import time
import webbrowser
from datetime import datetime, timezone
from typing import Any

import typer
from impreza import TopupInvoice
from impreza.exceptions import ApiError

from ..output import OutputFormat, error, print_dict, print_table, success
from ..output import info as info_msg  # `info` is also the verb name @app.command("info")
from ..sdk import make_client_or_exit
from ..state import from_typer_context, resolve_output
from ._helpers import exit_on_api_error as _exit_on_api_error

app = typer.Typer(
    name="account",
    help="Read your account profile, balance, and active services.",
    no_args_is_help=True,
)


# ── account info ─────────────────────────────────────────────────────


@app.command("info")
def info(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show your client profile and balance.

    Wraps ``GET /account``. The fields rendered are:
    name (first/last + optional company), email, balance + currency,
    and the date the account was registered.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            me = client.account.get()
        except ApiError as exc:
            _exit_on_api_error(exc)

    full_name = f"{me.first_name} {me.last_name}".strip()
    if me.company:
        full_name = f"{full_name} ({me.company})"

    data: dict[str, Any] = {
        "id": me.id,
        "name": full_name,
        "email": me.email,
        "balance": f"{me.balance:.2f} {me.currency}"
        if fmt is OutputFormat.TABLE
        else me.balance,
        "currency": me.currency,
        "registered_at": me.registered_at,
    }

    print_dict("Account", data, fmt=fmt)


# ── account balance ──────────────────────────────────────────────────


@app.command("balance")
def balance(
    typer_ctx: typer.Context,
    raw: bool = typer.Option(
        False,
        "--raw",
        help=(
            "Print just the numeric balance with no currency or formatting "
            "— useful in shell substitutions like `$(impreza account "
            "balance --raw)`."
        ),
    ),
) -> None:
    """Print the current account balance.

    Default output: ``45.32 USD`` on a single line, suitable for
    quick eyeballing. ``--raw`` strips the currency for shell
    arithmetic.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            me = client.account.get()
        except ApiError as exc:
            _exit_on_api_error(exc)

    if raw:
        # Single line, no trailing currency. Stays parseable by bc /
        # python -c / awk without futzing with split.
        typer.echo(f"{me.balance:.2f}")
    else:
        typer.echo(f"{me.balance:.2f} {me.currency}")


# ── account services ─────────────────────────────────────────────────


_SERVICE_COLUMNS = [
    "id",
    "domain",
    "product",
    "status",
    "billing_cycle",
    "amount",
    "next_due",
    "vps_backend",
]


@app.command("services")
def services(
    typer_ctx: typer.Context,
    status: str | None = typer.Option(
        None,
        "--status",
        help=(
            "Filter by service status (Active, Pending, Suspended, "
            "Cancelled, Terminated, Fraud). Case-insensitive at the API "
            "layer; passed through verbatim."
        ),
    ),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List the account's services across all backends.

    Wraps ``GET /account/services``. Each row carries the
    service id (use this with ``impreza vps show <id>`` etc.),
    domain, product name, status, billing cycle, recurring amount,
    next-due date, and the resolved ``vps_backend`` discriminator
    (``proxmox``, ``cloud``, or empty for non-VPS services).
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            items = client.account.services.list(status=status)
        except ApiError as exc:
            _exit_on_api_error(exc)

    rows: list[dict[str, Any]] = []
    for svc in items:
        rows.append(
            {
                "id": svc.id,
                "domain": svc.domain,
                "product": svc.product,
                "status": svc.status,
                "billing_cycle": svc.billing_cycle,
                "amount": f"{svc.amount:.2f}"
                if fmt is OutputFormat.TABLE
                else svc.amount,
                "next_due": svc.next_due,
                "vps_backend": svc.vps_backend,
            }
        )

    if not rows:
        msg = (
            f"No services with status {status!r} on this account."
            if status
            else "No services on this account yet."
        )
        typer.echo(msg)
        return

    title = (
        f"Services ({status})"
        if status
        else f"Services ({len(rows)} total)"
    )
    print_table(title, rows, columns=_SERVICE_COLUMNS, fmt=fmt)


# ── account topup (Phase 3.6, polished in 4.2) ──────────────────────


# Crypto confirmations are slow; matches the SDK's default in _topup.py.
_TOPUP_POLL_INTERVAL_SECONDS = 30.0

# Width to pad the in-place progress line to, so each redraw fully
# overwrites the previous one regardless of terminal width. 100 chars
# is wider than the longest line the formatter produces and short
# enough to fit standard terminal widths without wrapping.
_PROGRESS_PAD_WIDTH = 100


def _seconds_until_expiry(expires_at: str | None) -> float | None:
    """Parse an ISO 8601 timestamp and return seconds from now until
    that moment, in UTC. Returns ``None`` if the input is missing or
    unparseable — the caller decides whether to suppress the
    "until expiry" portion of the progress line in that case.

    Server emits the timestamp as ``"2026-05-11T20:00:00Z"`` (RFC
    3339-ish with Z suffix). Python's ``datetime.fromisoformat`` was
    extended to accept ``Z`` only in 3.11+; we still target 3.10+ so
    swap ``Z`` for ``+00:00`` before parsing for portability.
    """
    if not expires_at:
        return None
    iso = expires_at
    if iso.endswith("Z"):
        iso = iso[:-1] + "+00:00"
    try:
        target = datetime.fromisoformat(iso)
    except ValueError:
        return None
    if target.tzinfo is None:
        # Treat naive timestamps as UTC — matches what the server
        # actually emits even when the Z gets stripped by an
        # intermediate proxy.
        target = target.replace(tzinfo=timezone.utc)
    return (target - datetime.now(timezone.utc)).total_seconds()


def _format_progress_line(invoice: TopupInvoice, elapsed_s: float) -> str:
    """Render the single-line in-place progress message:
    ``Waiting on top-up invoice N — Xs elapsed / Ys until expiry``.

    The "until expiry" portion is appended only when ``expires_at``
    parses to a future timestamp. Past-expiry case omits it (the
    next poll iteration will hit the ``--timeout`` branch and
    surface the failure with the payment URL).
    """
    line = (
        f"Waiting on top-up invoice {invoice.invoice_id} "
        f"— {elapsed_s:.0f}s elapsed"
    )
    remaining = _seconds_until_expiry(invoice.expires_at)
    if remaining is not None and remaining > 0:
        line += f" / {remaining:.0f}s until expiry"
    return line


def _write_progress(line: str) -> None:
    """Write a carriage-return-redrawn progress line to stdout,
    padded so previous-iteration leftover characters are
    overwritten. ``flush()`` ensures the line appears immediately
    even when stdout is line-buffered.

    Test capture via ``CliRunner`` records every byte written so
    assertions can still find the latest line text in the
    accumulated output buffer.
    """
    sys.stdout.write("\r" + line.ljust(_PROGRESS_PAD_WIDTH))
    sys.stdout.flush()


def _clear_progress() -> None:
    """Erase the in-place progress line so the next ``typer.echo``
    starts on a clean row."""
    sys.stdout.write("\r" + " " * _PROGRESS_PAD_WIDTH + "\r")
    sys.stdout.flush()


def _wait_for_topup(
    invoice: TopupInvoice,
    *,
    timeout: int,
    poll_interval: float = _TOPUP_POLL_INTERVAL_SECONDS,
) -> None:
    """Block on a :class:`TopupInvoice` future, redrawing a single
    progress line each poll cycle so the user sees elapsed + ETA
    until invoice expiry without scrolling the terminal.

    Mirrors :func:`commands._helpers.wait_for_operation` for
    the TopupInvoice future. Crypto poll intervals default to 30s
    in the SDK — keep that here so the CLI doesn't hammer the
    gateway faster than the SDK would in programmatic use.

    The renderer uses bare ``sys.stdout`` (not ``typer.echo``)
    because the in-place ``\\r`` rewrite needs to skip Click's
    line buffering. Final state always lands as a clean settled
    line so the next renderer call (the post-wait
    ``_render_topup_invoice``) starts on a fresh row.
    """
    elapsed = 0.0
    while not invoice.is_done():
        if elapsed >= timeout:
            _clear_progress()
            error(
                f"Top-up invoice {invoice.invoice_id} not paid within "
                f"{timeout}s (last status: {invoice.status!r}). "
                "Re-run with a larger --timeout, or check the payment "
                f"URL: {invoice.payment_url or '(unknown)'}"
            )
            raise typer.Exit(code=1)
        _write_progress(_format_progress_line(invoice, elapsed))
        time.sleep(poll_interval)
        elapsed += poll_interval
        try:
            invoice.refresh()
        except ApiError as exc:
            _clear_progress()
            _exit_on_api_error(exc)
    _clear_progress()
    # Status-conditional rendering: paid → success (green); failed
    # gets the error() line below + we keep this neutral via info_msg().
    if invoice.is_paid():
        success(
            f"Top-up invoice {invoice.invoice_id} settled: "
            f"status={invoice.status!r}"
        )
    else:
        info_msg(
            f"Top-up invoice {invoice.invoice_id} settled: "
            f"status={invoice.status!r}"
        )
    if invoice.is_failed():
        error(
            f"Top-up invoice {invoice.invoice_id} ended in "
            f"{invoice.status!r}. Funds were not credited."
        )
        raise typer.Exit(code=1)


def _open_payment_url(invoice: TopupInvoice) -> None:
    """Best-effort: open the invoice's ``payment_url`` in the
    default browser, or print a friendly message if the URL is
    missing or the OS doesn't have a browser configured.

    Never raises. ``webbrowser.open()`` returns False when no
    browser was found, True when the OS spawned one — but on some
    headless Linux setups it raises instead, hence the broad
    except. Either way the user still has the URL printed by
    :func:`_render_topup_invoice` and can copy/paste it.
    """
    if not invoice.payment_url:
        info_msg(
            "  (no payment_url returned — invoice may already be "
            "settled or the gateway is misconfigured)"
        )
        return
    try:
        opened = webbrowser.open(invoice.payment_url)
    except Exception as exc:  # noqa: BLE001 — webbrowser raises bare Exception on headless
        info_msg(
            f"  (could not open browser: {exc}; copy the URL above to pay)"
        )
        return
    if opened:
        info_msg("  (payment URL opened in your default browser)")
    else:
        info_msg(
            "  (no default browser configured; copy the URL above to pay)"
        )


def _render_topup_invoice(
    invoice: TopupInvoice,
    *,
    fmt: OutputFormat,
    title: str,
) -> None:
    """Common renderer for ``topup`` and ``topup-status``. Picks
    fields that are useful in both states (just-created vs polled)."""
    data: dict[str, Any] = {
        "invoice_id": invoice.invoice_id,
        "amount": (
            f"{invoice.amount:.2f}"
            if fmt is OutputFormat.TABLE
            else invoice.amount
        ),
        "currency": invoice.currency,
        "method": invoice.method or "",
        "status": invoice.status,
        "payment_url": invoice.payment_url or "",
        "expires_at": invoice.expires_at or "",
        "paid_at": invoice.paid_at or "",
        "balance_after": (
            f"{invoice.balance_after:.2f}"
            if invoice.balance_after is not None and fmt is OutputFormat.TABLE
            else invoice.balance_after
        ),
    }
    print_dict(title, data, fmt=fmt)


@app.command("topup")
def topup(
    typer_ctx: typer.Context,
    amount: float = typer.Option(
        ...,
        "--amount", "-a",
        help=(
            "Amount to top up, in account currency (USD by default). "
            "The crypto-gateway converts to the chosen --method at the "
            "spot rate when the invoice is created."
        ),
        min=0.0,
    ),
    method: str | None = typer.Option(
        None,
        "--method", "-m",
        help=(
            "Optional crypto method hint (e.g. 'btc', 'xmr', "
            "'usdt-trc20'). Defaults to the gateway's default if "
            "omitted — the payment URL lets the customer pick at the "
            "BTCPay step either way."
        ),
    ),
    browser: bool = typer.Option(
        False,
        "--browser",
        help=(
            "Open the invoice's payment_url in the system browser "
            "immediately after the create call. Opt-in — the default "
            "flow stays scriptable. No-op when --output json is set "
            "since the JSON consumer typically handles the URL itself."
        ),
    ),
    wait: bool = typer.Option(
        False,
        "--wait",
        help=(
            "Block until the gateway confirms payment (or the invoice "
            "expires). Crypto confirmations are slow — default timeout "
            "matches the server-side invoice expiry of 2h. Progress "
            "renders in place with elapsed time + ETA until expiry."
        ),
    ),
    timeout: int = typer.Option(
        7200,
        "--timeout",
        help=(
            "Max seconds to wait when --wait is set. Default 7200 (2h) "
            "matches the server-side invoice expiry."
        ),
    ),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Create a crypto top-up invoice.

    Wraps ``c.account.topup(amount=..., method=...)``. The server
    creates an ``AddFunds`` invoice routed to the ``btcpayinline``
    gateway and returns a :class:`TopupInvoice` future with a
    ``payment_url``. Open the URL in a browser to pay; once the
    gateway confirms, your Impreza Account credits the balance automatically.

    Use ``--browser`` to skip the copy-paste step and open the
    payment URL automatically. Use ``--wait`` to block until paid
    (or until the 2h invoice expires); the progress renderer shows
    elapsed time and an ETA until expiry, redrawn in place.
    Without ``--wait`` the CLI prints the invoice details and
    exits — poll later with
    ``impreza account topup-status <invoice-id>``.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            invoice = client.account.topup(amount=amount, method=method)
        except ApiError as exc:
            _exit_on_api_error(exc)
            return

        # Render the freshly-created invoice (payment URL is critical here).
        _render_topup_invoice(
            invoice,
            fmt=fmt,
            title=f"Top-up invoice {invoice.invoice_id} (just created)",
        )

        # --browser kicks the OS browser at the payment_url. Suppressed
        # in JSON mode because the JSON consumer is a script that
        # presumably handles the URL itself; opening a browser would
        # be a surprise side effect.
        if browser and fmt is OutputFormat.TABLE:
            _open_payment_url(invoice)

        if not wait:
            return

        if invoice.is_done():
            # Edge case: gateway confirmed before we got here. Render
            # the final state and exit.
            typer.echo(
                f"Top-up invoice {invoice.invoice_id} is already "
                f"{invoice.status!r}."
            )
            return

        _wait_for_topup(invoice, timeout=timeout)
        # After wait, render the settled state.
        _render_topup_invoice(
            invoice,
            fmt=fmt,
            title=f"Top-up invoice {invoice.invoice_id} (settled)",
        )


@app.command("topup-status")
def topup_status(
    typer_ctx: typer.Context,
    invoice_id: int = typer.Argument(
        ...,
        help="Invoice id returned by `impreza account topup`.",
    ),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Check the current status of a top-up invoice.

    Wraps ``c.account.topup_status(invoice_id)``. Returns the latest
    gateway state without blocking. Note that ``payment_url`` and
    ``expires_at`` are not echoed by this endpoint (they're set
    once on creation) — they'll appear empty here.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            invoice = client.account.topup_status(invoice_id)
        except ApiError as exc:
            _exit_on_api_error(exc)
            return

    _render_topup_invoice(
        invoice,
        fmt=fmt,
        title=f"Top-up invoice {invoice_id}",
    )
