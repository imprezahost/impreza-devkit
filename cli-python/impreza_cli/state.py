"""CLI-wide shared state passed between the root callback and
subcommands via ``typer.Context.obj``.

Subcommands never read these flags from their own argument list —
they pull them off ``ctx.obj`` so the same flag can be set at the
``impreza`` root (`impreza --output json account info`) or at the
subcommand level (`impreza account info --output json`). Per-command
overrides win; falling back through to the context override and
then the table default.

The state object is intentionally tiny — only fields that are read
by more than one command should live here. Anything specific to a
single command stays as a regular function argument.
"""

from __future__ import annotations

from dataclasses import dataclass

import typer

from .output import OutputFormat


@dataclass
class GlobalState:
    """Values populated by the root callback and shared with every
    subcommand."""

    #: When set, the named context is used instead of the default.
    #: ``None`` means "use the config file's ``default_context``".
    context_override: str | None = None

    #: Default output format selected via the global ``--output`` flag.
    #: Per-command ``--output`` flags can override this on a single
    #: invocation; ``None`` here means "no preference, fall back to
    #: the table default".
    output: OutputFormat | None = None


def from_typer_context(ctx: typer.Context) -> GlobalState:
    """Pull the :class:`GlobalState` off a Typer context.

    Typer's :attr:`Context.obj` is typed as ``Any`` — this helper
    narrows it back to ``GlobalState`` so callers don't have to
    sprinkle ``cast()`` calls. If for any reason the obj is missing
    (test invocation that bypasses the root callback, etc.), an
    empty ``GlobalState`` is returned so commands don't NPE.
    """
    obj = getattr(ctx, "obj", None)
    if isinstance(obj, GlobalState):
        return obj
    return GlobalState()


def resolve_output(
    state: GlobalState,
    per_command: OutputFormat | None,
) -> OutputFormat:
    """Pick the output format for a single command invocation.

    Resolution order: per-command `--output` flag > global `--output`
    flag > :attr:`OutputFormat.TABLE` default.
    """
    if per_command is not None:
        return per_command
    if state.output is not None:
        return state.output
    return OutputFormat.TABLE


def confirm_or_exit(message: str, *, yes: bool) -> None:
    """Prompt the user to confirm a destructive / costly action.

    The standard pattern across every Phase 3 mutating command:

    .. code-block:: python

        confirm_or_exit("This will charge $12.99 from your balance.", yes=yes)

    When ``yes=True`` the prompt is skipped (intent already opted-in
    via the ``--yes`` flag). When the user declines, the CLI prints
    "Cancelled." on stdout and exits 0 — declining is not an error.

    The message should be a complete sentence describing what's
    about to happen; the helper appends "Continue?" automatically.
    """
    import typer  # local import keeps state.py importable without typer

    if yes:
        return
    if not typer.confirm(f"{message} Continue?", default=False):
        typer.echo("Cancelled.")
        raise typer.Exit(code=0)
