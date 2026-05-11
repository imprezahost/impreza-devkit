"""``impreza context`` subcommand surface.

Five operations:

* ``impreza context create <name> --key ... --secret ... [--base-url ...]
  [--default-output table|json|yaml] [--overwrite]``
* ``impreza context use <name>``
* ``impreza context list``
* ``impreza context current``
* ``impreza context delete <name> [--yes]``

The first context created becomes the default automatically. Names
must match ``[A-Za-z0-9_-]{1,50}`` (so they never need quoting in
shell commands).

The config file lives at ``$XDG_CONFIG_HOME/impreza/config.toml``
(Linux), ``~/Library/Application Support/impreza/config.toml`` (macOS),
or ``%APPDATA%\\impreza\\config.toml`` (Windows). Override with
``IMPREZA_CONFIG=/path/to/config.toml`` for tests or alternate
layouts.

Errors render as red ``Error:`` lines on stderr and exit non-zero so
scripts can detect failure cleanly. Stdout stays reserved for the
success output.
"""

from __future__ import annotations

import typer

from ..config import (
    Config,
    ConfigError,
    ContextAlreadyExists,
    ContextNotFound,
    InvalidContextName,
    NoActiveContext,
    NoContextsConfigured,
)
from ..output import OutputFormat, error, info, print_dict, print_table, success
from ..state import from_typer_context, resolve_output

app = typer.Typer(
    name="context",
    help="Manage local CLI contexts (named credential sets).",
    no_args_is_help=True,
)


# ── helpers ───────────────────────────────────────────────────────────


def _exit_on_config_error(exc: ConfigError) -> None:
    """Print a friendly message and exit 1. Used by every command's
    top-level error handler so error formatting stays consistent."""
    error(str(exc))
    raise typer.Exit(code=1)


def _mask_secret(value: str) -> str:
    """Render a secret string as ``imp_abc...xyz`` (first/last 4 chars
    of the suffix; never the middle). Used in human-readable output
    to confirm "yes the right key is loaded" without leaking it."""
    if len(value) <= 12:
        return "[dim]<short>[/]"
    return f"{value[:8]}…{value[-4:]}"


# ── commands ──────────────────────────────────────────────────────────


@app.command("create")
def create(
    name: str = typer.Argument(..., help="Context name (alphanumeric, '-', '_')."),
    key: str = typer.Option(..., "--key", "-k", help="API key (`imp_...`)."),
    secret: str = typer.Option(
        ..., "--secret", "-s", help="API secret (64 hex chars).",
    ),
    base_url: str | None = typer.Option(
        None,
        "--base-url",
        help="Optional API base URL override (defaults to "
        "https://api.imprezahost.com/v1).",
    ),
    default_output: OutputFormat | None = typer.Option(
        None,
        "--default-output",
        help="Optional default output format for this context.",
        case_sensitive=False,
    ),
    overwrite: bool = typer.Option(
        False,
        "--overwrite",
        help="Replace an existing context with the same name.",
    ),
) -> None:
    """Create a new context. The first one created is auto-set as
    the default."""
    cfg = Config.load()
    try:
        ctx = cfg.add_context(
            name,
            api_key=key,
            api_secret=secret,
            base_url=base_url,
            default_output=default_output.value if default_output else None,
            overwrite=overwrite,
        )
    except (ContextAlreadyExists, InvalidContextName) as exc:
        _exit_on_config_error(exc)
    cfg.save()

    is_default = cfg.default_context == name
    success(
        f"Context {ctx.name!r} created"
        + (" and set as default." if is_default else ".")
    )


@app.command("use")
def use(
    name: str = typer.Argument(..., help="Context name to switch to."),
) -> None:
    """Set the default context."""
    cfg = Config.load()
    try:
        cfg.use_context(name)
    except ContextNotFound as exc:
        _exit_on_config_error(exc)
    cfg.save()
    success(f"Now using context {name!r}.")


@app.command("list")
def list_contexts(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag for this command.",
        case_sensitive=False,
    ),
) -> None:
    """List all configured contexts with their masked credentials."""
    fmt = resolve_output(from_typer_context(typer_ctx), output)
    cfg = Config.load()
    rows = [
        {
            "name": ctx.name,
            "default": cfg.default_context == ctx.name,
            "api_key": _mask_secret(ctx.api_key)
            if fmt is OutputFormat.TABLE
            else ctx.api_key,
            "base_url": ctx.base_url,
            "default_output": ctx.default_output,
        }
        for ctx in (cfg.contexts[name] for name in cfg.list_contexts())
    ]
    if not rows:
        typer.echo("No contexts configured. Run `impreza context create <name>`.")
        return
    print_table(
        "Contexts",
        rows,
        columns=["name", "default", "api_key", "base_url", "default_output"],
        fmt=fmt,
    )


@app.command("current")
def current(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag for this command.",
        case_sensitive=False,
    ),
) -> None:
    """Show the active (default) context's metadata. Exits non-zero
    when no contexts are configured or no default is set."""
    fmt = resolve_output(from_typer_context(typer_ctx), output)
    cfg = Config.load()
    try:
        ctx = cfg.get_context()
    except (NoContextsConfigured, NoActiveContext, ContextNotFound) as exc:
        _exit_on_config_error(exc)

    print_dict(
        "Current context",
        {
            "name": ctx.name,
            "api_key": _mask_secret(ctx.api_key)
            if fmt is OutputFormat.TABLE
            else ctx.api_key,
            "base_url": ctx.base_url or "<default>",
            "default_output": ctx.default_output or "<inherit>",
        },
        fmt=fmt,
    )


@app.command("delete")
def delete(
    name: str = typer.Argument(..., help="Context name to delete."),
    yes: bool = typer.Option(
        False,
        "--yes",
        "-y",
        help="Skip the interactive confirmation prompt.",
    ),
) -> None:
    """Delete a context. Prompts for confirmation unless ``--yes``."""
    cfg = Config.load()
    if name not in cfg.contexts:
        _exit_on_config_error(ContextNotFound(f"Context {name!r} does not exist."))

    if not yes and not typer.confirm(f"Delete context {name!r}?", default=False):
        typer.echo("Cancelled.")
        raise typer.Exit(code=0)

    was_default = cfg.default_context == name
    cfg.remove_context(name)
    cfg.save()

    success(f"Context {name!r} deleted.")
    if was_default and cfg.contexts:
        info(
            "  (no default context now — pick one with `impreza context use <name>`.)"
        )
