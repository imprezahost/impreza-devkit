"""``impreza key`` subcommand surface — Phase 2.6.

Currently one verb:

* ``impreza key whoami [-o ...]``
    Wraps ``c.account.api_key_self()`` (which itself wraps
    ``GET /v1/account/api-keys/self``). Prints the active API
    key's identity — public prefix, label, status, last-used
    timestamp, rate limit, and the IP whitelist as a sub-table.
    The full secret is never returned by the server, so no
    masking is needed here.

Key management verbs (`create` / `revoke` / `list`) hit the Impreza Account
client area portal API rather than this addon's surface, so they
live in a separate fase that wires the portal API as an SDK
resource.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError

from ..output import OutputFormat, print_dict, print_table
from ..sdk import make_client_or_exit
from ..state import from_typer_context, resolve_output
from ._helpers import exit_on_api_error as _exit_on_api_error

app = typer.Typer(
    name="key",
    help="Inspect the active API key's identity and IP whitelist.",
    no_args_is_help=True,
)


@app.command("whoami")
def whoami(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Identity of the API key making this call.

    Useful for confirming which context's credentials are loaded
    and which IP the server is observing — common debugging
    scenario when a request is rejected with `IP_NOT_WHITELISTED`.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            ident = client.account.api_key_self()
        except ApiError as exc:
            _exit_on_api_error(exc)

    if fmt is not OutputFormat.TABLE:
        print_dict("API key identity", ident.model_dump(), fmt=fmt)
        return

    # Table mode: Field/Value header + sub-table for the whitelist.
    header: dict[str, Any] = {
        "id": ident.id,
        "client_id": ident.client_id,
        "prefix": ident.prefix,
        "label": ident.label,
        "status": ident.status,
        "last_used_at": ident.last_used_at,
        "created_at": ident.created_at,
        "rate_limit_per_minute": ident.rate_limit_per_minute,
        "request_ip": ident.request_ip,
    }
    print_dict("API key identity", header, fmt=fmt)

    if ident.ip_whitelist:
        rows = [
            {
                "id": ip.id,
                "ip_address": ip.ip_address,
                "label": ip.label,
                "current": ip.ip_address == ident.request_ip,
                "created_at": ip.created_at,
            }
            for ip in ident.ip_whitelist
        ]
        print_table(
            f"IP whitelist ({len(rows)})",
            rows,
            columns=["id", "ip_address", "label", "current", "created_at"],
            fmt=fmt,
        )
    else:
        # No whitelist entries means "this key has no IP restrictions"
        # — surface that clearly. Server enforces auth via the key
        # secret regardless, but no whitelist is unusual on a real
        # account.
        typer.echo("(no IP whitelist configured on this key)")
