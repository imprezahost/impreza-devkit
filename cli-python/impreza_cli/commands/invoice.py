"""``impreza invoice`` subcommand surface — Phase 2.6.

Read-only invoice commands over `c.invoices.*`:

* ``impreza invoice list [--status STATUS] [-o ...]``
    Wraps ``c.invoices.list(status=...)``. Multi-row table with
    id / status / date / due_date / total / currency.
* ``impreza invoice show <id> [-o ...]``
    Wraps ``c.invoices.get(id)``. Field/value detail with line
    items as a sub-table (or JSON-array key in non-table modes).

Pay-from-balance (`POST /invoices/{id}/pay`) lands in Phase 3
alongside the rest of the mutating CLI surface.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError, ResourceNotFound

from ..output import OutputFormat, error, print_dict, print_table
from ..sdk import make_client_or_exit
from ..state import from_typer_context, resolve_output
from ._helpers import exit_on_api_error as _exit_on_api_error

app = typer.Typer(
    name="invoice",
    help="Read invoices on your account.",
    no_args_is_help=True,
)


_LIST_COLUMNS = ["id", "invoice_num", "status", "date", "due_date", "total"]


# ── invoice list ────────────────────────────────────────────────────


@app.command("list")
def list_invoices(
    typer_ctx: typer.Context,
    status: str | None = typer.Option(
        None,
        "--status",
        help=(
            "Filter by invoice status (Unpaid, Paid, Cancelled, "
            "Refunded, Collections). Forwarded verbatim to the API; "
            "case is preserved on the wire."
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
    """List invoices on the authenticated client's account.

    Wraps ``GET /invoices``. The current API caps the response at
    100 entries per call (no pagination metadata yet — tracked as
    a 1.3 follow-up). When the account has more than 100 invoices,
    only the most recent 100 come back; use your Impreza Account
    for full history until pagination ships.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            invoices = client.invoices.list(status=status)
        except ApiError as exc:
            _exit_on_api_error(exc)

    if not invoices:
        if status:
            typer.echo(f"No invoices with status {status!r} on this account.")
        else:
            typer.echo("No invoices on this account yet.")
        return

    if fmt is OutputFormat.TABLE:
        rows: list[dict[str, Any]] = [
            {
                "id": inv.id,
                "invoice_num": inv.invoice_num,
                "status": inv.status,
                "date": inv.date,
                "due_date": inv.due_date,
                "total": f"{inv.total:.2f}",
            }
            for inv in invoices
        ]
        title = (
            f"Invoices ({len(rows)} {status!r})"
            if status
            else f"Invoices ({len(rows)} total)"
        )
        print_table(title, rows, columns=_LIST_COLUMNS, fmt=fmt)
    else:
        rows = [inv.model_dump() for inv in invoices]
        print_table("Invoices", rows, fmt=fmt)


# ── invoice show ────────────────────────────────────────────────────


@app.command("show")
def show(
    typer_ctx: typer.Context,
    invoice_id: int = typer.Argument(..., help="Invoice id to inspect."),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show full invoice detail with line items and transactions.

    Wraps ``GET /invoices/{id}``. Table mode renders the invoice
    summary as a Field/Value table and follows it with sub-tables
    for line items and recorded transactions. JSON / YAML emit
    the full :class:`InvoiceDetail` model with items / transactions
    as nested arrays.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            inv = client.invoices.get(invoice_id)
        except ResourceNotFound:
            error(f"Invoice {invoice_id} not found on this account.")
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            _exit_on_api_error(exc)

    if fmt is not OutputFormat.TABLE:
        # Single JSON / YAML object with items + transactions nested.
        print_dict(f"Invoice {invoice_id}", inv.model_dump(), fmt=fmt)
        return

    # Table mode: header dict + sub-tables for items & transactions.
    header: dict[str, Any] = {
        "id": inv.id,
        "invoice_num": inv.invoice_num,
        "status": inv.status,
        "date": inv.date,
        "due_date": inv.due_date,
        "date_paid": inv.date_paid,
        "subtotal": f"{inv.subtotal:.2f}",
        "credit": f"{inv.credit:.2f}",
        "tax": f"{inv.tax:.2f}",
        "total": f"{inv.total:.2f}",
        "payment_method": inv.payment_method,
    }
    print_dict(f"Invoice {invoice_id}", header, fmt=fmt)

    if inv.items:
        item_rows = [
            {
                "id": it.id,
                "type": it.type,
                "description": it.description,
                "amount": f"{it.amount:.2f}",
                "taxed": it.taxed,
            }
            for it in inv.items
        ]
        print_table(
            f"Line items ({len(item_rows)})",
            item_rows,
            columns=["id", "type", "description", "amount", "taxed"],
            fmt=fmt,
        )

    if inv.transactions:
        tx_rows = [
            {
                "id": t.id,
                "date": t.date,
                "gateway": t.gateway,
                "amount": f"{t.amount:.2f}" if t.amount is not None else "-",
                "transaction_id": t.transaction_id,
            }
            for t in inv.transactions
        ]
        print_table(
            f"Transactions ({len(tx_rows)})",
            tx_rows,
            columns=["id", "date", "gateway", "amount", "transaction_id"],
            fmt=fmt,
        )
