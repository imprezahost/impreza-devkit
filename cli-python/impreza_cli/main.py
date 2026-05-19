"""Entry point — `impreza` console script.

Mounts every subcommand module from :mod:`impreza_cli.commands` and
exposes the global flags (``--version``, ``--context``, ``--output``)
on the root callback. Each subcommand pulls the global state off
``ctx.obj`` (see :mod:`impreza_cli.state`).

Phase 2.1 shipped the ``context`` subcommand. Phase 2.2 added
``account`` and the global ``--context`` / ``--output`` flags.
Subsequent fases (2.3+) mount more resource groups via the
``app.add_typer(...)`` block at the bottom of this file.
"""

from __future__ import annotations

import typer

from . import __version__
from .commands import account as account_cmd
from .commands import catalog as catalog_cmd
from .commands import context as context_cmd
from .commands import dedicated as dedicated_cmd
from .commands import doctor as doctor_cmd
from .commands import domain as domain_cmd
from .commands import invoice as invoice_cmd
from .commands import key as key_cmd
from .commands import orders as orders_cmd
from .commands import services as services_cmd
from .commands import vps as vps_cmd
from .commands import webhooks as webhooks_cmd
from .output import OutputFormat
from .state import GlobalState

app = typer.Typer(
    name="impreza",
    help="Official CLI for the Impreza Host public REST API.",
    no_args_is_help=True,
    rich_markup_mode="rich",
)


def _version_callback(value: bool) -> None:
    if value:
        typer.echo(f"impreza-cli {__version__}")
        raise typer.Exit()


@app.callback()
def _root(
    ctx: typer.Context,
    version: bool = typer.Option(
        False,
        "--version",
        callback=_version_callback,
        is_eager=True,
        help="Show the installed CLI version and exit.",
    ),
    context: str | None = typer.Option(
        None,
        "--context",
        "-c",
        help="Override the default context for this invocation.",
    ),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help=(
            "Default output format. Per-command --output flags override "
            "this. Falls back to 'table' when neither is set."
        ),
        case_sensitive=False,
    ),
) -> None:
    """Run ``impreza --help`` for the full command tree.

    Global flags ``--context`` and ``--output`` apply to every
    subcommand and are inherited via Typer's context object.
    """
    ctx.obj = GlobalState(context_override=context, output=output)


# ── Subcommand mounts ────────────────────────────────────────────────
#
# New resource groups land here. Keep them alphabetised so the
# ``impreza --help`` output reads predictably.
app.add_typer(account_cmd.app, name="account")
app.add_typer(catalog_cmd.app, name="catalog")
app.add_typer(context_cmd.app, name="context")
app.add_typer(dedicated_cmd.app, name="dedicated")
app.add_typer(doctor_cmd.app, name="doctor")
app.add_typer(domain_cmd.app, name="domain")
app.add_typer(invoice_cmd.app, name="invoice")
app.add_typer(key_cmd.app, name="key")
app.add_typer(orders_cmd.app, name="order")
app.add_typer(services_cmd.app, name="service")
app.add_typer(vps_cmd.app, name="vps")
app.add_typer(webhooks_cmd.app, name="webhook")


if __name__ == "__main__":
    app()
