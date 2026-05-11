"""``impreza order`` subcommand surface — Phase 3.6.

Four verbs over the SDK's :class:`~impreza.resources.orders.OrdersResource`
(shipped in 1.4d). Two read, two write — pure CLI work over methods
that already exist:

* ``impreza order list [--status STATUS]``
  Wraps ``c.orders.list(status=...)``. Up to 50 most recent orders,
  most recent first.

* ``impreza order show <id>``
  Wraps ``c.orders.get(id)``. Renders the order summary plus its
  line items as a follow-up table in table mode; JSON / YAML emit
  the full :class:`OrderDetail` payload.

* ``impreza order create --product-id N --billing-cycle CYCLE
  [--domain DOM] [--hostname HOST] [--config-option K=V ...]
  [--custom-field K=V ...] [--yes]``
  Wraps ``c.orders.create(...)``. Smart name/id resolution from
  1.4d carries through — pass ``--config-option "Disk Space=20 GB"``
  by name or ``--config-option 3=5`` by ID. Same for
  ``--custom-field``. Costs real money — gated by
  ``confirm_or_exit`` since the balance is debited.

* ``impreza order upgrade --service-id N --new-product-id M
  --billing-cycle CYCLE [--yes]``
  Wraps ``c.orders.upgrade(...)``. Charges the prorated difference
  from the client's balance. Same ``confirm_or_exit`` gate.

There's no ``impreza order cancel``: the SDK doesn't expose
``c.orders.cancel()`` because order cancellation in Impreza Account is
actually service cancellation via ``AddCancelRequest`` (the same
verb VPS already exposes as ``vps cancel``). The non-VPS
equivalent — ``impreza service cancel`` — lands in Phase 3.7.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError, InsufficientCredit, InvalidRequest

from ..output import OutputFormat, error, print_dict, print_table, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context, resolve_output
from ._helpers import exit_on_api_error

app = typer.Typer(
    name="order",
    help="Browse orders and submit new product / upgrade orders.",
    no_args_is_help=True,
)


_VALID_CYCLES = {
    "monthly",
    "quarterly",
    "semiannually",
    "annually",
    "biennially",
    "triennially",
}


# ── helpers ─────────────────────────────────────────────────────────


def _exit_on_insufficient_credit(exc: InsufficientCredit) -> None:
    """Hint at ``impreza account topup`` (3.6) so users can chain the
    fix without re-reading docs. Same pattern as 3.1's hint on
    domain-purchase errors."""
    parts = [exc.message]
    if exc.code:
        parts.append(f"(code={exc.code})")
    if exc.request_id:
        parts.append(f"[request_id={exc.request_id}]")
    error(" ".join(parts))
    error("→ Top up your balance with: impreza account topup --amount X")
    raise typer.Exit(code=1)


def _parse_kv_option(
    name: str,
    raw: list[str],
) -> dict[int | str, int | str]:
    """Parse repeated ``--config-option K=V`` flags into the dict shape
    the SDK accepts. Both K and V can be quoted strings or stringified
    integers; the SDK then resolves names or trusts IDs as needed.

    The conversion rule: if K (or V) parses as a Python int, treat it
    as an ID; otherwise it's a name. This means quoting matters only
    when a config-option name happens to be all digits — unlikely in
    practice but worth flagging in ``--help``.
    """
    out: dict[int | str, int | str] = {}
    for entry in raw:
        if "=" not in entry:
            error(
                f"--{name} expects 'KEY=VALUE' format, got: {entry!r}"
            )
            raise typer.Exit(code=1)
        k, _, v = entry.partition("=")
        k = k.strip()
        v = v.strip()
        if not k or not v:
            error(f"--{name} entry has an empty side: {entry!r}")
            raise typer.Exit(code=1)
        # Try int first; fall back to string.
        try:
            key: int | str = int(k)
        except ValueError:
            key = k
        try:
            val: int | str = int(v)
        except ValueError:
            val = v
        out[key] = val
    return out


def _parse_custom_fields(raw: list[str]) -> dict[int | str, str]:
    """Like :func:`_parse_kv_option` but values are always strings
    (custom-field values are free-form text)."""
    out: dict[int | str, str] = {}
    for entry in raw:
        if "=" not in entry:
            error(f"--custom-field expects 'KEY=VALUE' format, got: {entry!r}")
            raise typer.Exit(code=1)
        k, _, v = entry.partition("=")
        k = k.strip()
        v = v.strip()
        if not k:
            error(f"--custom-field entry has an empty key: {entry!r}")
            raise typer.Exit(code=1)
        try:
            key: int | str = int(k)
        except ValueError:
            key = k
        out[key] = v
    return out


# ── order list ──────────────────────────────────────────────────────


_LIST_COLUMNS = [
    "id", "order_number", "date", "amount",
    "status", "invoice_id", "payment_method",
]


@app.command("list")
def list_orders(
    typer_ctx: typer.Context,
    status: str | None = typer.Option(
        None,
        "--status",
        help=(
            "Filter by order status (Pending, Active, Cancelled, "
            "Fraud). Case-sensitive — match the canonical labels."
        ),
    ),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List up to the 50 most recent orders on this account.

    Wraps ``c.orders.list(status=...)``. Returns orders most-recent
    first. Use ``impreza order show <id>`` to fetch line items.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            orders = client.orders.list(status=status)
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not orders:
        if status:
            typer.echo(f"No orders match status {status!r}.")
        else:
            typer.echo("No orders on this account.")
        return

    rows = [
        {
            "id": o.id,
            "order_number": o.order_number if o.order_number is not None else "",
            "date": o.date or "",
            "amount": f"{o.amount:.2f}" if fmt is OutputFormat.TABLE else o.amount,
            "status": o.status,
            "invoice_id": o.invoice_id if o.invoice_id is not None else "",
            "payment_method": o.payment_method or "",
        }
        for o in orders
    ]
    title = f"Orders ({len(rows)}"
    if status:
        title += f", status={status!r}"
    title += ")"
    print_table(title, rows, columns=_LIST_COLUMNS, fmt=fmt)


# ── order show ──────────────────────────────────────────────────────


@app.command("show")
def show_order(
    typer_ctx: typer.Context,
    order_id: int = typer.Argument(..., help="Order id."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show full detail for one order, including its line items.

    Wraps ``c.orders.get(id)``. Table mode renders the order summary
    first, then the line items table; JSON / YAML emit the full
    :class:`OrderDetail` model.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            order = client.orders.get(order_id)
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if fmt is OutputFormat.TABLE:
        summary: dict[str, Any] = {
            "id": order.id,
            "order_number": order.order_number if order.order_number is not None else "",
            "date": order.date or "",
            "amount": f"{order.amount:.2f}",
            "status": order.status,
            "invoice_id": order.invoice_id if order.invoice_id is not None else "",
            "payment_method": order.payment_method or "",
        }
        print_dict(f"Order {order_id}", summary, fmt=fmt)
        if order.items:
            item_rows = [
                {
                    "service_id": it.service_id,
                    "domain": it.domain or "",
                    "product": it.product or "",
                    "status": it.status or "",
                    "billing_cycle": it.billing_cycle or "",
                    "amount": f"{it.amount:.2f}" if it.amount is not None else "",
                }
                for it in order.items
            ]
            print_table(
                f"Line items ({len(item_rows)})",
                item_rows,
                columns=["service_id", "domain", "product", "status",
                         "billing_cycle", "amount"],
                fmt=fmt,
            )
    else:
        # JSON / YAML: emit the full order with items inlined.
        data: dict[str, Any] = {
            "id": order.id,
            "order_number": order.order_number,
            "date": order.date,
            "amount": order.amount,
            "status": order.status,
            "invoice_id": order.invoice_id,
            "payment_method": order.payment_method,
            "items": [
                {
                    "service_id": it.service_id,
                    "domain": it.domain,
                    "product": it.product,
                    "status": it.status,
                    "billing_cycle": it.billing_cycle,
                    "amount": it.amount,
                }
                for it in order.items
            ],
        }
        print_dict(f"Order {order_id}", data, fmt=fmt)


# ── order create ────────────────────────────────────────────────────


@app.command("create")
def create_order(
    typer_ctx: typer.Context,
    product_id: int = typer.Option(
        ...,
        "--product-id", "-p",
        help="Product id from `impreza catalog products`.",
    ),
    billing_cycle: str = typer.Option(
        ...,
        "--billing-cycle", "-b",
        help=(
            "Billing cycle: monthly / quarterly / semiannually / "
            "annually / biennially / triennially. Validated client-side."
        ),
    ),
    domain: str | None = typer.Option(
        None,
        "--domain",
        help="Domain name to associate with the service (when applicable).",
    ),
    hostname: str | None = typer.Option(
        None,
        "--hostname",
        help="Hostname (e.g. for VPS services). Optional.",
    ),
    config_option: list[str] = typer.Option(
        [],
        "--config-option", "-c",
        help=(
            "Configurable option: 'KEY=VALUE'. Pass multiple times. KEY "
            "and VALUE can be names (e.g. 'Disk Space=20 GB') or IDs "
            "(e.g. '3=5'). Names trigger one extra GET /products/{id} "
            "call for resolution; IDs skip resolution."
        ),
    ),
    custom_field: list[str] = typer.Option(
        [],
        "--custom-field", "-f",
        help=(
            "Custom field value: 'KEY=VALUE'. Pass multiple times. KEY "
            "can be a name or ID (same rule as --config-option); the "
            "value is always a free-form string."
        ),
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the balance-debit confirmation prompt."
    ),
) -> None:
    """Create a new order. **Charges your account balance.**

    Wraps ``c.orders.create(...)``. The order's cost is debited from
    your credit balance — if the balance is insufficient, the SDK
    raises :class:`InsufficientCredit` (HTTP 402) which the CLI maps
    to a friendly stderr line plus a hint to run
    ``impreza account topup`` (3.6).

    Smart name/id resolution from 1.4d: ``--config-option`` and
    ``--custom-field`` accept either ID-keyed or name-keyed entries.
    Name resolution costs one extra GET /products/{id} call.
    """
    if billing_cycle not in _VALID_CYCLES:
        error(
            f"--billing-cycle must be one of {sorted(_VALID_CYCLES)!r}, "
            f"got: {billing_cycle!r}"
        )
        raise typer.Exit(code=1)

    config_opts = _parse_kv_option("config-option", config_option)
    custom_flds = _parse_custom_fields(custom_field)

    state = from_typer_context(typer_ctx)
    msg = (
        f"Creating order for product {product_id} on a "
        f"{billing_cycle} cycle. Cost will be charged from your "
        "account balance."
    )
    if domain:
        msg = msg.rstrip(".") + f" Domain: {domain!r}."
    confirm_or_exit(msg, yes=yes)

    with make_client_or_exit(state) as client:
        try:
            result = client.orders.create(
                product_id=product_id,
                billing_cycle=billing_cycle,
                domain=domain,
                hostname=hostname,
                config_options=config_opts or None,
                custom_fields=custom_flds or None,
            )
        except InsufficientCredit as exc:
            _exit_on_insufficient_credit(exc)
            return
        except InvalidRequest as exc:
            # UNKNOWN_OPTION / UNKNOWN_FIELD from the SDK's resolver
            # already carry friendly messages; pass through.
            exit_on_api_error(exc)
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    success(
        f"Order {result.order_id} created: "
        f"invoice {result.invoice_id}, "
        f"{result.amount:.2f} {result.currency}, status={result.status!r}"
        + (f" — {result.product!r}" if result.product else "")
    )


# ── order upgrade ───────────────────────────────────────────────────


@app.command("upgrade")
def upgrade_order(
    typer_ctx: typer.Context,
    service_id: int = typer.Option(
        ...,
        "--service-id", "-s",
        help="Service id of the service being upgraded.",
    ),
    new_product_id: int = typer.Option(
        ...,
        "--new-product-id", "-p",
        help="Product id of the target product.",
    ),
    billing_cycle: str = typer.Option(
        ...,
        "--billing-cycle", "-b",
        help=(
            "Target billing cycle: monthly / quarterly / semiannually / "
            "annually / biennially / triennially."
        ),
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the balance-debit confirmation prompt."
    ),
) -> None:
    """Upgrade an existing service to a different product / cycle.
    **Charges the prorated difference** from your balance.

    Wraps ``c.orders.upgrade(service_id, new_product_id, billing_cycle)``.
    Note: the upstream does NOT yet support changing
    config_options / custom_fields on upgrade — only the product and
    billing cycle. Existing customizations carry through.
    """
    if billing_cycle not in _VALID_CYCLES:
        error(
            f"--billing-cycle must be one of {sorted(_VALID_CYCLES)!r}, "
            f"got: {billing_cycle!r}"
        )
        raise typer.Exit(code=1)

    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Upgrading service {service_id} to product {new_product_id} "
        f"on a {billing_cycle} cycle will charge the prorated "
        "difference from your account balance.",
        yes=yes,
    )

    with make_client_or_exit(state) as client:
        try:
            result = client.orders.upgrade(
                service_id=service_id,
                new_product_id=new_product_id,
                billing_cycle=billing_cycle,
            )
        except InsufficientCredit as exc:
            _exit_on_insufficient_credit(exc)
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    success(
        f"Service {service_id} upgrade order {result.order_id} created: "
        f"invoice {result.invoice_id}, "
        f"{result.amount:.2f} {result.currency}, status={result.status!r}"
    )
