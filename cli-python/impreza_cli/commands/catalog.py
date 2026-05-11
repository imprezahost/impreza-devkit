"""``impreza catalog`` subcommand surface — Phase 2.3.

Three verbs reading from :class:`impreza.resources.catalog.CatalogResource`:

* ``impreza catalog products [--group X] [--type T]``
* ``impreza catalog product-groups``
* ``impreza catalog tlds [--filter .com,.net,...]``

The catalog is reference data — values change only when staff edit
Impreza Account, not as a side effect of customer activity. These commands are
the discovery layer customers use before placing orders (Phase 3).

VPS-Cloud catalog (sizes / locations) is deliberately deferred —
the underlying Cloud backend endpoints exist but return a deeply-
nested provider-specific shape that warrants its own focused pass
once Phase 3 ordering creates real demand for it.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError
from impreza.models.product import Product
from impreza.models.tld import TldPricing

from ..output import OutputFormat, print_table
from ..sdk import make_client_or_exit
from ..state import from_typer_context, resolve_output
from ._helpers import exit_on_api_error as _exit_on_api_error

app = typer.Typer(
    name="catalog",
    help="Browse the product catalog, product groups, and TLD pricing.",
    no_args_is_help=True,
)


# ── catalog products ─────────────────────────────────────────────────


def _cheapest_cycle(product: Product) -> str:
    """Format the cheapest billing cycle as a single-cell table value
    (``monthly: 5.00``). Returns ``"-"`` for products with no
    pricing configured (free or quote-only)."""
    if not product.pricing:
        return "-"
    cycle, price = min(
        product.pricing.items(),
        key=lambda kv: kv[1].price,
    )
    return f"{cycle}: {price.price:.2f}"


@app.command("products")
def products(
    typer_ctx: typer.Context,
    group: str | None = typer.Option(
        None,
        "--group",
        "-g",
        help="Filter by product group name (case-insensitive substring match upstream).",
    ),
    type_: str | None = typer.Option(
        None,
        "--type",
        "-t",
        help=(
            "Filter by product type. One of: hostingaccount, "
            "reselleraccount, server, other."
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
    """List products in the catalog with their cheapest cycle price.

    Wraps ``GET /products``. Table mode shows id / name / group /
    type / currency / cheapest-cycle. JSON / YAML modes emit the full
    Product model (including the per-cycle pricing dict) so callers
    can pick out a specific cycle for ordering.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            items = client.catalog.products(group=group, type=type_)
        except ApiError as exc:
            _exit_on_api_error(exc)

    if not items:
        if group or type_:
            filters = []
            if group:
                filters.append(f"group={group!r}")
            if type_:
                filters.append(f"type={type_!r}")
            typer.echo(f"No products match the filter: {', '.join(filters)}.")
        else:
            typer.echo("No products in the catalog yet.")
        return

    if fmt is OutputFormat.TABLE:
        rows: list[dict[str, Any]] = [
            {
                "id": p.id,
                "name": p.name,
                "group": p.group,
                "type": p.type,
                "currency": p.currency,
                "cheapest": _cheapest_cycle(p),
            }
            for p in items
        ]
        print_table(
            f"Products ({len(rows)})",
            rows,
            columns=["id", "name", "group", "type", "currency", "cheapest"],
            fmt=fmt,
        )
    else:
        # JSON / YAML: emit the full model — by_alias=False, so callers
        # see Python attribute names (`register_prices` etc.). The
        # Pydantic dump preserves the per-cycle pricing dict so callers
        # can pick a specific cycle for ordering.
        rows = [p.model_dump() for p in items]
        print_table("Products", rows, fmt=fmt)


# ── catalog product-groups ───────────────────────────────────────────


@app.command("product-groups")
def product_groups(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List product groups with the count of products in each.

    Wraps ``GET /products/groups``. Useful before
    ``impreza catalog products --group <name>`` to find the right
    filter value.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            groups = client.catalog.product_groups()
        except ApiError as exc:
            _exit_on_api_error(exc)

    if not groups:
        typer.echo("No product groups defined yet.")
        return

    rows = [
        {"id": g.id, "name": g.name, "product_count": g.product_count}
        for g in groups
    ]
    print_table(
        f"Product groups ({len(rows)})",
        rows,
        columns=["id", "name", "product_count"],
        fmt=fmt,
    )


# ── catalog tlds ─────────────────────────────────────────────────────


def _year_1_price(prices: dict[str, float]) -> float | None:
    """Pull out the 1-year price from a ``{"1": x, "2": y}`` map.
    Returns None when 1-year isn't offered (some registrars require
    multi-year minimums)."""
    raw = prices.get("1")
    return float(raw) if raw is not None else None


@app.command("tlds")
def tlds(
    typer_ctx: typer.Context,
    filter_: str | None = typer.Option(
        None,
        "--filter",
        "-f",
        help=(
            "Comma-separated list of TLDs (e.g. '.com,.net,.io'). "
            "Without a filter, the full TLD catalog is returned."
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
    """List domain TLD pricing.

    Wraps ``GET /domains/pricing``. Table mode shows tld / currency /
    1-year register / 1-year renew / cheapest-overall. JSON / YAML
    emit the full :class:`TldPricing` model with per-year pricing
    dicts for both register and renew.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            items: list[TldPricing] = client.catalog.tlds(filter=filter_)
        except ApiError as exc:
            _exit_on_api_error(exc)

    if not items:
        msg = (
            f"No TLDs match the filter: {filter_!r}."
            if filter_
            else "No TLDs in the catalog yet."
        )
        typer.echo(msg)
        return

    if fmt is OutputFormat.TABLE:
        rows: list[dict[str, Any]] = []
        for t in items:
            reg_1y = _year_1_price(t.register_prices)
            ren_1y = _year_1_price(t.renew_prices)
            rows.append(
                {
                    "tld": t.tld,
                    "currency": t.currency,
                    "register_1y": f"{reg_1y:.2f}" if reg_1y is not None else "-",
                    "renew_1y": f"{ren_1y:.2f}" if ren_1y is not None else "-",
                    "cheapest": (
                        f"{t.cheapest:.2f}" if t.cheapest is not None else "-"
                    ),
                    "min_years": t.min_years,
                }
            )
        print_table(
            f"TLDs ({len(rows)})",
            rows,
            columns=[
                "tld",
                "currency",
                "register_1y",
                "renew_1y",
                "cheapest",
                "min_years",
            ],
            fmt=fmt,
        )
    else:
        rows = [t.model_dump(by_alias=True) for t in items]
        print_table("TLDs", rows, fmt=fmt)
