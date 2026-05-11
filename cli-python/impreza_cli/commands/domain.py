"""``impreza domain`` subcommand surface.

Read commands shipped in 2.4 (`show / check / pricing` + `dns list`).
Write commands shipped in 3.1: domain registration / transfer /
nameservers / lock / id-protection / RAA / GDPR / transfer-approval
plus DNS CRUD on the `dns` sub-app.

``impreza domain list`` is still deferred — the server has no
listing endpoint and the SDK has no ``c.domains.list()`` method.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError, InsufficientCredit, ResourceNotFound

from ..output import OutputFormat, error, info, print_dict, print_table, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context, resolve_output
from ._helpers import exit_on_api_error as _exit_on_api_error

app = typer.Typer(
    name="domain",
    help="Read domain registrations, check availability, and inspect DNS.",
    no_args_is_help=True,
)

# Sub-app for DNS commands. `dns list` shipped in 2.4; CRUD and
# `dns activate` land in 3.1.
dns_app = typer.Typer(
    name="dns",
    help="Inspect and manage DNS records on registered domains.",
    no_args_is_help=True,
)
app.add_typer(dns_app, name="dns")


def _exit_on_insufficient_credit(exc: InsufficientCredit) -> None:
    """402 Insufficient Credit gets a special message that points
    at the topup command, since "add money to your balance" is the
    standard remediation. Once Phase 3 ships ``impreza account
    topup`` (3.6), users can chain the suggestion directly."""
    parts = [exc.message]
    if exc.code:
        parts.append(f"(code={exc.code})")
    error(" ".join(parts))
    error(
        "  -> Top up your balance with: "
        "impreza account topup --amount <X> --method btc|xmr|trx|usdt"
    )
    raise typer.Exit(code=1)


def _try_lookup_register_price(
    client: Any, domain: str, years: int
) -> tuple[float, str] | None:
    """Best-effort price lookup so the confirmation prompt can show
    "$X.XX from your balance" instead of "<unknown cost>".

    Returns ``None`` (silent fall-through) if the catalog call fails
    or the TLD isn't priced — registration still works, we just
    don't show the cost up front.
    """
    try:
        # Extract the TLD (".com", ".net", ".com.br") for the filter.
        tld = "." + domain.split(".", 1)[1] if "." in domain else None
        if not tld:
            return None
        tlds = client.catalog.tlds(filter=tld)
        if not tlds:
            return None
        prices = tlds[0].register_prices
        per_year = prices.get(str(years)) or prices.get("1")
        if per_year is None:
            return None
        # If only year-1 is priced and caller wants more years,
        # multiply naively. Real price for N years may differ;
        # surface this caveat in the prompt by including "≈".
        if str(years) not in prices and years > 1:
            per_year = float(per_year) * years
        return float(per_year), tlds[0].currency
    except Exception:  # noqa: BLE001 - best-effort, never block the order on this
        return None


def _year_1_price(prices: dict[str, float]) -> float | None:
    raw = prices.get("1")
    return float(raw) if raw is not None else None


# ── domain show ──────────────────────────────────────────────────────


@app.command("show")
def show(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain name to inspect (e.g. example.com)."),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show full registration details for a domain.

    Wraps ``GET /domains/{domain}``. Renders status / expiry /
    nameservers / lock state / ID protection / auto-renew. Most
    fields are nullable upstream — table mode shows ``-`` for
    unknown values rather than blanking the row.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            d = client.domains.get(domain)
        except ResourceNotFound:
            error(f"Domain {domain!r} is not registered to this account.")
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            _exit_on_api_error(exc)

    data: dict[str, Any] = {
        "domain": d.domain,
        "status": d.status,
        "registration_date": d.registration_date,
        "expires_at": d.expires_at,
        "next_due_date": d.next_due_date,
        "nameservers": (
            ", ".join(d.nameservers)
            if fmt is OutputFormat.TABLE and d.nameservers
            else d.nameservers
        ),
        "lock_status": d.lock_status,
        "id_protection": d.id_protection,
        "auto_renew": d.auto_renew,
        "privacy": d.privacy,
        "epp_code": d.epp_code,
    }
    print_dict(f"Domain {domain}", data, fmt=fmt)


# ── domain check ─────────────────────────────────────────────────────


@app.command("check")
def check(
    typer_ctx: typer.Context,
    domains: list[str] = typer.Argument(
        ...,
        help="One or more domain names to check (max 10 per call).",
    ),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Check availability for one or more domains in a single call.

    Wraps ``GET /domains/check?domains=...``. The server caps the
    batch at 10 domains per call — passing more raises an SDK
    validation error before the round-trip. Table output sorts the
    results by domain name; JSON / YAML preserve the input order.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            availability = client.domains.check(list(domains))
        except ApiError as exc:
            _exit_on_api_error(exc)

    if fmt is OutputFormat.TABLE:
        rows = [
            {"domain": name, "available": availability.get(name, False)}
            for name in sorted(availability.keys())
        ]
        print_table(
            f"Availability ({len(rows)})",
            rows,
            columns=["domain", "available"],
            fmt=fmt,
        )
    else:
        # Stable input-order list for JSON; consumers shouldn't have
        # to re-sort to align with the request.
        rows = [
            {"domain": name, "available": availability.get(name, False)}
            for name in domains
        ]
        print_table("Availability", rows, fmt=fmt)


# ── domain pricing ───────────────────────────────────────────────────


@app.command("pricing")
def pricing(
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
    """Show TLD register / renew pricing.

    Functionally equivalent to ``impreza catalog tlds`` — same SDK
    call, same render, mounted in the ``domain`` namespace for
    muscle memory (people thinking about domain registrations
    naturally type `impreza domain pricing`). The catalog version
    stays the canonical entry point.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            tlds = client.catalog.tlds(filter=filter_)
        except ApiError as exc:
            _exit_on_api_error(exc)

    if not tlds:
        msg = (
            f"No TLDs match the filter: {filter_!r}."
            if filter_
            else "No TLDs in the catalog yet."
        )
        typer.echo(msg)
        return

    if fmt is OutputFormat.TABLE:
        rows: list[dict[str, Any]] = []
        for t in tlds:
            reg_1y = _year_1_price(t.register_prices)
            ren_1y = _year_1_price(t.renew_prices)
            rows.append(
                {
                    "tld": t.tld,
                    "currency": t.currency,
                    "register_1y": (
                        f"{reg_1y:.2f}" if reg_1y is not None else "-"
                    ),
                    "renew_1y": (
                        f"{ren_1y:.2f}" if ren_1y is not None else "-"
                    ),
                    "cheapest": (
                        f"{t.cheapest:.2f}" if t.cheapest is not None else "-"
                    ),
                    "min_years": t.min_years,
                }
            )
        print_table(
            f"Domain pricing ({len(rows)})",
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
        rows = [t.model_dump(by_alias=True) for t in tlds]
        print_table("Domain pricing", rows, fmt=fmt)


# ── domain dns list ──────────────────────────────────────────────────


@dns_app.command("list")
def dns_list(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain whose DNS records to list."),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List all DNS records for a domain.

    Wraps ``GET /domains/{domain}/dns``. The domain must have DNS
    management activated (``c.domains.activate_dns(domain)``). Empty
    record lists are valid — a freshly-activated domain renders as
    a friendly "no records" message rather than an empty table.

    Write verbs (``add`` / ``update`` / ``delete``) land in Phase 3
    alongside the rest of the mutating CLI surface.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            records = client.domains.dns.list(domain)
        except ResourceNotFound:
            error(
                f"Domain {domain!r} not found, or DNS management is not "
                "active on it. Activate first with: impreza domain dns "
                "activate <domain>."
            )
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            _exit_on_api_error(exc)

    if not records:
        typer.echo(f"No DNS records on {domain!r} yet.")
        return

    rows = [
        {
            "type": r.type,
            "host": r.host,
            "value": r.value,
            "ttl": r.ttl,
            "priority": r.priority,
        }
        for r in records
    ]
    print_table(
        f"DNS records — {domain} ({len(rows)})",
        rows,
        columns=["type", "host", "value", "ttl", "priority"],
        fmt=fmt,
    )


# ═══════════════════════════════════════════════════════════════════
#                            WRITES (Phase 3.1)
# ═══════════════════════════════════════════════════════════════════


# ── domain register ─────────────────────────────────────────────────


@app.command("register")
def register(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to register (e.g. example.com)."),
    years: int = typer.Option(1, "--years", help="Registration period (1-10)."),
    nameservers: list[str] | None = typer.Option(
        None,
        "--ns",
        "--nameserver",
        help=(
            "Repeat to set nameservers at registration time. Defaults "
            "to Impreza nameservers when not supplied."
        ),
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the cost-confirmation prompt."
    ),
) -> None:
    """Register a new domain. Pays from account balance.

    Wraps ``c.domains.register()``. Looks up the registration price
    via the catalog before prompting so users see the charge upfront.
    On success, prints the order id + invoice id.
    """
    state = from_typer_context(typer_ctx)

    with make_client_or_exit(state) as client:
        # Best-effort price lookup. Falls through silently if the
        # TLD isn't priced — the registration still works.
        price = _try_lookup_register_price(client, domain, years)

        # Build the confirmation message with whatever pricing info
        # we have. Always include the amount when available.
        if price is not None:
            amount, currency = price
            try:
                me = client.account.get()
                balance_msg = f" (balance: {me.balance:.2f} {me.currency})"
            except ApiError:
                balance_msg = ""
            msg = (
                f"Register {domain!r} for {years} year(s) "
                f"— {amount:.2f} {currency} from your balance{balance_msg}."
            )
        else:
            msg = (
                f"Register {domain!r} for {years} year(s). "
                "Cost will be charged from your account balance."
            )
        confirm_or_exit(msg, yes=yes)

        try:
            result = client.domains.register(
                domain=domain, years=years, nameservers=nameservers
            )
        except InsufficientCredit as exc:
            _exit_on_insufficient_credit(exc)
        except ApiError as exc:
            _exit_on_api_error(exc)

    success(
        f"Registered {result.domain!r} — "
        f"order #{result.order_id}, invoice #{result.invoice_id}, "
        f"charged {result.amount:.2f} {result.currency}."
    )


# ── domain transfer ─────────────────────────────────────────────────


@app.command("transfer")
def transfer(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to transfer in."),
    epp: str = typer.Option(
        ..., "--epp", help="Authorisation / EPP code from the current registrar."
    ),
    years: int = typer.Option(1, "--years", help="Renewal period to add (default 1)."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the cost-confirmation prompt."
    ),
) -> None:
    """Transfer a domain in. Pays from account balance.

    Wraps ``c.domains.transfer()``. The EPP code is required and
    must come from the losing registrar. Most TLDs have a 5-7 day
    transfer window during which the gaining registrar (Impreza)
    contacts the losing one — see ``impreza domain show <d>`` for
    progress after transfer is initiated.
    """
    state = from_typer_context(typer_ctx)

    with make_client_or_exit(state) as client:
        price = _try_lookup_register_price(client, domain, years)
        if price is not None:
            amount, currency = price
            msg = (
                f"Transfer {domain!r} (renewal: {years} year) "
                f"— ~{amount:.2f} {currency} from your balance."
            )
        else:
            msg = (
                f"Transfer {domain!r} (renewal: {years} year). "
                "Cost will be charged from your account balance."
            )
        confirm_or_exit(msg, yes=yes)

        try:
            result = client.domains.transfer(
                domain=domain, epp_code=epp, years=years
            )
        except InsufficientCredit as exc:
            _exit_on_insufficient_credit(exc)
        except ApiError as exc:
            _exit_on_api_error(exc)

    success(
        f"Transfer initiated for {result.domain!r} — "
        f"order #{result.order_id}, invoice #{result.invoice_id}, "
        f"charged {result.amount:.2f} {result.currency}. "
        "Check progress with: impreza domain show "
        f"{result.domain}"
    )


# ── domain set-nameservers ──────────────────────────────────────────


@app.command("set-nameservers")
def set_nameservers(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to update."),
    nameservers: list[str] = typer.Argument(
        ...,
        help="Nameserver hostnames (minimum 2). Pass each as a positional arg.",
    ),
) -> None:
    """Replace the domain's nameservers (minimum 2).

    Wraps ``c.domains.set_nameservers()``. Propagation across the
    DNS hierarchy can take up to 48h after this returns; the
    registry is updated immediately.
    """
    state = from_typer_context(typer_ctx)
    if len(nameservers) < 2:
        error("at least 2 nameservers are required")
        raise typer.Exit(code=1)

    with make_client_or_exit(state) as client:
        try:
            client.domains.set_nameservers(domain, list(nameservers))
        except ApiError as exc:
            _exit_on_api_error(exc)

    success(
        f"Nameservers for {domain!r} set to: {', '.join(nameservers)}."
    )


# ── domain lock / unlock ────────────────────────────────────────────


@app.command("lock")
def lock(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to lock."),
) -> None:
    """Enable transfer lock. Prevents the domain from being
    transferred to another registrar without first unlocking.

    Wraps ``c.domains.lock()``.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            client.domains.lock(domain)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"Transfer lock enabled on {domain!r}.")


@app.command("unlock")
def unlock(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to unlock."),
    yes: bool = typer.Option(False, "--yes", "-y", help="Skip the warning prompt."),
) -> None:
    """Disable transfer lock and return the EPP / authorisation
    code needed to initiate a transfer at another registrar.

    Wraps ``c.domains.unlock()``. Unlocking is a security-sensitive
    action — anyone with the EPP code can pull your domain — so we
    confirm by default. Re-lock with ``impreza domain lock`` after
    you're done.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Unlocking {domain!r} returns the EPP code, which authorises "
        "transfers away from Impreza. Anyone with the code can move "
        "the domain.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        try:
            epp = client.domains.unlock(domain)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"Transfer lock disabled on {domain!r}.")
    info(f"  EPP / auth code: {epp}")


# ── domain id-protection ────────────────────────────────────────────


@app.command("id-protection")
def id_protection(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to protect."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the cost-confirmation prompt."
    ),
) -> None:
    """Purchase WHOIS Privacy / ID protection. Pays from account
    balance.

    Wraps ``c.domains.purchase_id_protection()``. Hides the
    registrant contact details from public WHOIS lookups. Some TLDs
    (e.g. ``.us``, certain ccTLDs) don't support privacy at the
    registry level — the API returns 400 with a descriptive
    message in that case.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Purchase ID protection for {domain!r}. "
        "Cost will be charged from your account balance.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        try:
            result = client.domains.purchase_id_protection(domain)
        except InsufficientCredit as exc:
            _exit_on_insufficient_credit(exc)
        except ApiError as exc:
            _exit_on_api_error(exc)

    # The SDK returns the raw `data` dict — fields vary by upstream
    # response; surface the whole thing as a small key/value table
    # so users see whatever the registrar reported.
    if isinstance(result, dict) and result:
        rows = {str(k): v for k, v in result.items()}
        print_dict(f"ID protection — {domain}", rows, fmt=OutputFormat.TABLE)
    else:
        success(f"ID protection purchased for {domain!r}.")


# ── domain raa-verify / gdpr-auth / transfer-approval ───────────────


@app.command("raa-verify")
def raa_verify(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain awaiting RAA verification."),
) -> None:
    """Resend the ICANN RAA email-verification message.

    Wraps ``c.domains.resend_raa_verification()``. Required after
    registration to confirm the registrant email address; without
    it, ICANN suspends the domain after 15 days.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            client.domains.resend_raa_verification(domain)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"RAA verification email resent for {domain!r}.")


@app.command("gdpr-auth")
def gdpr_auth(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain awaiting GDPR authorisation."),
) -> None:
    """Resend the GDPR data-processing authorisation email.

    Wraps ``c.domains.resend_gdpr_auth()``. Required for EU-resident
    registrants on certain TLDs.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            client.domains.resend_gdpr_auth(domain)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"GDPR authorisation email resent for {domain!r}.")


@app.command("transfer-approval")
def transfer_approval(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain awaiting transfer approval."),
) -> None:
    """Resend the inbound-transfer approval email.

    Wraps ``c.domains.resend_transfer_approval()``. Sent to the
    registrant's WHOIS email by the gaining registrar; users
    sometimes miss it, this command resends.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            client.domains.resend_transfer_approval(domain)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"Transfer approval email resent for {domain!r}.")


# ═══════════════════════════════════════════════════════════════════
#                       DNS CRUD (Phase 3.1)
# ═══════════════════════════════════════════════════════════════════


_DNS_TYPES = ["A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV"]


# ── dns activate ────────────────────────────────────────────────────


@dns_app.command("activate")
def dns_activate(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to activate DNS management on."),
) -> None:
    """Activate DNS management on a registered domain.

    Wraps ``c.domains.activate_dns()``. Required exactly once before
    ``dns add`` / ``update`` / ``delete`` will succeed. After this,
    the registry's NS records point at Impreza's nameservers and
    record CRUD becomes available via the API.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            client.domains.activate_dns(domain)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"DNS management activated on {domain!r}.")


# ── dns add ─────────────────────────────────────────────────────────


@dns_app.command("add")
def dns_add(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain to add the record to."),
    type_: str = typer.Option(
        ...,
        "--type",
        help=f"Record type. One of: {', '.join(_DNS_TYPES)}.",
    ),
    name: str = typer.Option(
        ...,
        "--name",
        help='Record host. Use "@" for the apex.',
    ),
    value: str = typer.Option(..., "--value", help="Record value."),
    ttl: int | None = typer.Option(
        None, "--ttl", help="TTL in seconds. Server default if omitted."
    ),
    priority: int | None = typer.Option(
        None,
        "--priority",
        help="Required for MX; ignored for other types.",
    ),
) -> None:
    """Add a DNS record.

    Wraps ``c.domains.dns.add()``. The SDK rejects invalid record
    types client-side, so a typo in ``--type`` errors before any
    network round-trip.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            client.domains.dns.add(
                domain,
                type=type_,
                host=name,
                value=value,
                ttl=ttl,
                priority=priority,
            )
        except ValueError as exc:
            error(str(exc))
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(
        f"Added {type_} record on {domain!r}: {name} -> {value}"
        + (f" (TTL {ttl}s)" if ttl else "")
        + (f" priority={priority}" if priority is not None else "")
    )


# ── dns update ──────────────────────────────────────────────────────


@dns_app.command("update")
def dns_update(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain whose record to update."),
    type_: str = typer.Option(..., "--type", help="Record type."),
    name: str = typer.Option(..., "--name", help='Record host (use "@" for apex).'),
    old_value: str = typer.Option(
        ...,
        "--old-value",
        help="Current value. Used as the match key — must equal what's currently in DNS.",
    ),
    new_value: str = typer.Option(..., "--new-value", help="Replacement value."),
    ttl: int | None = typer.Option(None, "--ttl", help="New TTL in seconds (optional)."),
    priority: int | None = typer.Option(
        None, "--priority", help="New MX priority (optional, MX only)."
    ),
) -> None:
    """Update a DNS record by matching ``type + name + old-value``.

    Wraps ``c.domains.dns.update()``. The match is exact —
    case-sensitive on host, exact-byte on value. If the record
    doesn't match, the API returns 404 (caught and surfaced).
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            client.domains.dns.update(
                domain,
                type=type_,
                host=name,
                old_value=old_value,
                new_value=new_value,
                ttl=ttl,
                priority=priority,
            )
        except ValueError as exc:
            error(str(exc))
            raise typer.Exit(code=1) from None
        except ResourceNotFound:
            error(
                f"No matching {type_} record on {domain!r} with "
                f"name={name!r} value={old_value!r}."
            )
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(
        f"Updated {type_} record on {domain!r}: "
        f"{name} -> {old_value!r} replaced with {new_value!r}."
    )


# ── dns delete ──────────────────────────────────────────────────────


@dns_app.command("delete")
def dns_delete(
    typer_ctx: typer.Context,
    domain: str = typer.Argument(..., help="Domain whose record to delete."),
    type_: str = typer.Option(..., "--type", help="Record type."),
    name: str = typer.Option(..., "--name", help='Record host (use "@" for apex).'),
    value: str = typer.Option(..., "--value", help="Record value (exact match required)."),
    yes: bool = typer.Option(False, "--yes", "-y", help="Skip the confirmation prompt."),
) -> None:
    """Delete a DNS record by exact match of ``type + name + value``.

    Wraps ``c.domains.dns.delete()``. Confirms by default since the
    deletion is immediate and can break dependent services (e.g.
    deleting an MX record affects mail delivery).
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Delete {type_} record on {domain!r}: {name} -> {value!r}.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        try:
            client.domains.dns.delete(domain, type=type_, host=name, value=value)
        except ValueError as exc:
            error(str(exc))
            raise typer.Exit(code=1) from None
        except ResourceNotFound:
            error(
                f"No matching {type_} record on {domain!r} with "
                f"name={name!r} value={value!r}."
            )
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"Deleted {type_} record on {domain!r}: {name} -> {value!r}.")
