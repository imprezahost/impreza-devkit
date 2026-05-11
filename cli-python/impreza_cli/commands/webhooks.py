"""``impreza webhook`` subcommand surface — Phase 3.7.

Eight verbs over the :class:`~impreza.WebhooksResource` shipped in
1.6. All pure CLI work — the SDK side and server side are both
complete since Phase 0 (server) and Phase 1.6 (SDK).

* ``webhook list`` — every subscription on the account.
* ``webhook show <id>`` — one subscription's detail.
* ``webhook create --url URL --event TYPE [--event TYPE ...]
  [--description D]`` — register a new subscription. The HMAC
  secret is printed ONCE on this call and ONLY on this call (and
  on ``rotate-secret``). Subsequent reads return null for the
  secret field.
* ``webhook update <id> [--url ...] [--event TYPE ...]
  [--description ...] [--activate/--deactivate]`` — PATCH semantics;
  pass only the fields to change.
* ``webhook delete <id> [--yes]`` — remove the subscription.
  Pending undelivered events are dropped at the server.
* ``webhook rotate-secret <id>`` — generate a fresh HMAC secret.
  The previous secret stops working immediately; the new one is
  printed ONCE.
* ``webhook deliveries <id>`` — up to 100 recent delivery
  attempts for one subscription.
* ``webhook event-types`` — the catalog of subscribable event
  types + wildcard patterns.

Event types are passed via repeated ``--event TYPE`` flags rather
than a comma-separated string so quoting edge cases (events with
periods, glob patterns like ``vps.*``) don't bite. The SDK accepts
wildcards (``"vps.*"``, ``"*"``) verbatim.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError

from ..output import OutputFormat, error, print_dict, print_table, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context, resolve_output
from ._helpers import exit_on_api_error

app = typer.Typer(
    name="webhook",
    help="Manage webhook subscriptions and inspect delivery history.",
    no_args_is_help=True,
)


# ── helpers ─────────────────────────────────────────────────────────


def _subscription_row(sub: Any, *, fmt: OutputFormat) -> dict[str, Any]:
    """Lift a WebhookSubscription into a flat row. Table mode joins
    the events list with commas; JSON / YAML keep the list shape."""
    events_field = (
        ", ".join(sub.events)
        if fmt is OutputFormat.TABLE
        else list(sub.events)
    )
    return {
        "id": sub.id,
        "url": sub.url,
        "events": events_field,
        "description": sub.description or "",
        "is_active": "yes" if sub.is_active else "no",
        "last_delivery_at": sub.last_delivery_at or "",
        "last_delivery_status": (
            sub.last_delivery_status
            if sub.last_delivery_status is not None
            else ""
        ),
        "created_at": sub.created_at or "",
    }


_LIST_COLUMNS = [
    "id", "url", "events", "is_active",
    "last_delivery_at", "last_delivery_status",
]


# ── webhook list ────────────────────────────────────────────────────


@app.command("list")
def list_webhooks(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List every webhook subscription on the account.

    Wraps ``c.webhooks.list()``. The ``secret`` field is null on
    all entries — it's only returned on create / rotate-secret.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            subs = client.webhooks.list()
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not subs:
        typer.echo("No webhook subscriptions on this account.")
        return

    rows = [_subscription_row(s, fmt=fmt) for s in subs]
    print_table(
        f"Webhook subscriptions ({len(rows)})",
        rows,
        columns=_LIST_COLUMNS,
        fmt=fmt,
    )


# ── webhook show ────────────────────────────────────────────────────


@app.command("show")
def show_webhook(
    typer_ctx: typer.Context,
    subscription_id: int = typer.Argument(..., help="Subscription id."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show one subscription's detail.

    Wraps ``c.webhooks.get(id)``.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            sub = client.webhooks.get(subscription_id)
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    data = _subscription_row(sub, fmt=fmt)
    print_dict(f"Webhook subscription {subscription_id}", data, fmt=fmt)


# ── webhook create ──────────────────────────────────────────────────


@app.command("create")
def create_webhook(
    typer_ctx: typer.Context,
    url: str = typer.Option(
        ...,
        "--url", "-u",
        help="HTTPS URL the server will POST events to.",
    ),
    event: list[str] = typer.Option(
        ...,
        "--event", "-e",
        help=(
            "Event type or wildcard. Pass multiple times. Examples: "
            "`--event topup.paid --event vps.*` (concrete + wildcard); "
            "`--event '*'` (everything). Run `webhook event-types` to "
            "list the catalog."
        ),
    ),
    description: str | None = typer.Option(
        None, "--description", "-d",
        help="Optional human-readable note for the subscription.",
    ),
) -> None:
    """Create a new webhook subscription.

    Wraps ``c.webhooks.create(url=..., events=[...],
    description=...)``. The HMAC secret is printed **only on this
    call** (and on ``rotate-secret``). Capture it before the
    terminal closes — there's no way to retrieve it later other
    than rotating.
    """
    state = from_typer_context(typer_ctx)

    if not event:
        error("--event requires at least one event type (pass it multiple times).")
        raise typer.Exit(code=1)

    with make_client_or_exit(state) as client:
        try:
            sub = client.webhooks.create(
                url=url, events=event, description=description
            )
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    success(f"Subscription {sub.id} created: {sub.url}")
    typer.echo(f"  Events: {', '.join(sub.events)}")
    if sub.secret:
        typer.echo("")
        typer.echo(f"  HMAC SECRET (shown only once): {sub.secret}")
        if sub.secret_warning:
            typer.echo(f"  WARNING: {sub.secret_warning}")
        typer.echo("  Store this securely; rotate with `webhook rotate-secret`.")


# ── webhook update ──────────────────────────────────────────────────


@app.command("update")
def update_webhook(
    typer_ctx: typer.Context,
    subscription_id: int = typer.Argument(..., help="Subscription id."),
    url: str | None = typer.Option(
        None, "--url", "-u", help="New URL (optional)."
    ),
    event: list[str] = typer.Option(
        [],
        "--event", "-e",
        help=(
            "Replace the event list with these tokens. Pass multiple "
            "times. Pass at least one ``--event`` flag to update; "
            "omit entirely to leave events untouched. (PATCH semantics.)"
        ),
    ),
    description: str | None = typer.Option(
        None, "--description", "-d", help="New description (optional)."
    ),
    activate: bool = typer.Option(
        False, "--activate", help="Set is_active=true."
    ),
    deactivate: bool = typer.Option(
        False, "--deactivate", help="Set is_active=false."
    ),
) -> None:
    """Update one or more fields on a subscription. **PATCH** — only
    the flags you pass get updated.

    Wraps ``c.webhooks.update(id, **kwargs)``. ``--activate`` and
    ``--deactivate`` are mutually exclusive. If you pass neither and
    no other flag, the SDK rejects the empty body with a ValueError
    — the CLI surfaces that with a clear stderr line.
    """
    if activate and deactivate:
        error("--activate and --deactivate are mutually exclusive.")
        raise typer.Exit(code=1)

    is_active: bool | None = None
    if activate:
        is_active = True
    elif deactivate:
        is_active = False

    events_arg = event if event else None

    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        try:
            sub = client.webhooks.update(
                subscription_id,
                url=url,
                events=events_arg,
                description=description,
                is_active=is_active,
            )
        except ValueError as exc:
            # SDK raises ValueError when no field was set to update
            error(str(exc))
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    success(
        f"Subscription {sub.id} updated: {sub.url} "
        f"(events: {', '.join(sub.events)}, "
        f"is_active={sub.is_active})"
    )


# ── webhook delete ──────────────────────────────────────────────────


@app.command("delete")
def delete_webhook(
    typer_ctx: typer.Context,
    subscription_id: int = typer.Argument(..., help="Subscription id."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the deletion confirmation prompt."
    ),
) -> None:
    """Delete a subscription. **Irreversible.** Pending undelivered
    events are dropped at the server.

    Wraps ``c.webhooks.delete(id)``.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Deleting webhook subscription {subscription_id} is "
        "irreversible — pending undelivered events are dropped at "
        "the server. Re-creating the subscription gets a fresh "
        "secret and a new id.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        try:
            client.webhooks.delete(subscription_id)
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Webhook subscription {subscription_id} deleted.")


# ── webhook rotate-secret ───────────────────────────────────────────


@app.command("rotate-secret")
def rotate_secret(
    typer_ctx: typer.Context,
    subscription_id: int = typer.Argument(..., help="Subscription id."),
    yes: bool = typer.Option(
        False, "--yes", "-y",
        help="Skip the rotation confirmation prompt.",
    ),
) -> None:
    """Generate a fresh HMAC secret for a subscription. **The
    previous secret stops working immediately.**

    Wraps ``c.webhooks.rotate_secret(id)``. The new secret is
    printed only on this call — capture it before the terminal
    closes.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Rotating the secret for subscription {subscription_id} "
        "invalidates the previous secret immediately. Receivers "
        "still verifying with the old secret will reject deliveries "
        "until you update them with the new one.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        try:
            secret = client.webhooks.rotate_secret(subscription_id)
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not secret:
        # Server didn't echo the secret — surface a clear failure
        # instead of silently printing an empty line.
        error(
            f"Secret rotation succeeded but no secret was returned. "
            f"This shouldn't happen; check subscription "
            f"{subscription_id} via `webhook show` and contact "
            f"support if rotation didn't actually take effect."
        )
        raise typer.Exit(code=1)
    success(f"Secret rotated for subscription {subscription_id}:")
    typer.echo(f"  HMAC SECRET (shown only once): {secret}")
    typer.echo("  Update every receiver verifying with the previous secret.")


# ── webhook deliveries ──────────────────────────────────────────────


_DELIVERY_COLUMNS = [
    "id", "event_type", "event_id", "attempts",
    "last_attempted_at", "last_response_code",
    "delivered", "delivered_at",
]


@app.command("deliveries")
def deliveries(
    typer_ctx: typer.Context,
    subscription_id: int = typer.Argument(..., help="Subscription id."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List recent delivery attempts (up to 100) for a subscription.

    Wraps ``c.webhooks.deliveries(id)``. Useful for debugging why a
    receiver isn't getting events — the ``last_error`` and
    ``last_response_code`` columns surface what the server saw on
    the most recent attempt.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            history = client.webhooks.deliveries(subscription_id)
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not history:
        typer.echo(
            f"No delivery history yet for subscription {subscription_id}."
        )
        return

    rows = [
        {
            "id": d.id,
            "event_type": d.event_type,
            "event_id": d.event_id,
            "attempts": d.attempts,
            "last_attempted_at": d.last_attempted_at or "",
            "last_response_code": (
                d.last_response_code if d.last_response_code is not None else ""
            ),
            "delivered": "yes" if d.delivered else "no",
            "delivered_at": d.delivered_at or "",
        }
        for d in history
    ]
    print_table(
        f"Deliveries for subscription {subscription_id} ({len(rows)})",
        rows,
        columns=_DELIVERY_COLUMNS,
        fmt=fmt,
    )


# ── webhook event-types ─────────────────────────────────────────────


@app.command("event-types")
def event_types(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show the catalog of subscribable event types and wildcards.

    Wraps ``c.webhooks.event_types()``. Use this to discover which
    event names a receiver can subscribe to — the list grows as the
    server adds events, so the SDK / CLI never hardcode a copy.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            catalog = client.webhooks.event_types()
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if fmt is OutputFormat.TABLE:
        if not catalog.event_types and not catalog.wildcards:
            typer.echo("No event types configured upstream.")
            return
        if catalog.event_types:
            typer.echo("Event types:")
            for ev in catalog.event_types:
                typer.echo(f"  - {ev}")
        if catalog.wildcards:
            typer.echo("")
            typer.echo("Wildcards:")
            for pat, desc in catalog.wildcards.items():
                typer.echo(f"  {pat}  -  {desc}")
    else:
        data: dict[str, Any] = {
            "event_types": list(catalog.event_types),
            "wildcards": dict(catalog.wildcards),
        }
        print_dict("Webhook event-type catalog", data, fmt=fmt)
