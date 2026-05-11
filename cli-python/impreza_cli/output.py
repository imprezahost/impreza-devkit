"""Output formatting for CLI commands.

The CLI supports three output modes selected via the global
``--output`` flag (default: ``table``):

* ``table`` — Rich-rendered tables for human consumption (default).
* ``json``  — UTF-8 JSON, pretty-printed with 2-space indent. Pipes
  cleanly into ``jq`` and friends.
* ``yaml``  — defer-imported PyYAML output (only loaded when actually
  selected so the import cost is paid only on use).

Phase 2.1 ships only ``table`` and ``json`` since the context
commands' output is small. ``yaml`` lands in Phase 2.7 alongside the
output polish + tab completion pass; the ``OutputFormat`` enum
already lists it so command code can target the final API today and
the 2.7 work is purely additive.

Stderr is a separate channel via :func:`error` — used for
user-visible errors (typed config failures, etc.) so the success
output on stdout stays scriptable.
"""

from __future__ import annotations

import json
import sys
from enum import Enum
from typing import Any

import typer
from rich.console import Console
from rich.table import Table

__all__ = [
    "OutputFormat",
    "error",
    "info",
    "print_dict",
    "print_table",
    "success",
    "warning",
]


class OutputFormat(str, Enum):
    """Selectable output mode. ``str``-mixin so it round-trips
    cleanly through Typer's `--output` flag."""

    TABLE = "table"
    JSON = "json"
    YAML = "yaml"


_stdout = Console()


def error(message: str) -> None:
    """Print a user-facing error message to stderr in red.

    Uses ``typer.secho`` (line-based, via Click) rather than Rich
    so the output doesn't wrap based on detected terminal width.
    Wrapping was hiding the back half of long error messages
    (e.g. ``[request_id=...]`` suffixes) from stderr capture in
    CliRunner-driven tests, and would do the same to grep-piped
    output on narrow real terminals.

    Bugs should still raise so the traceback isn't swallowed —
    this helper is only for friendly errors on expected failures.
    """
    typer.secho(f"Error: {message}", err=True, fg=typer.colors.RED, bold=True)


def success(message: str) -> None:
    """Print a user-facing success message to stdout in green.

    Companion to :func:`error`. Use for the trailing "X created /
    deleted / updated" line that confirms an operation took effect.
    No "OK:" prefix — Phase 1.6's cp1252 lesson kept us off Unicode
    glyphs and the colour itself reads as success cue without one.

    Note: success messages go to stdout (not stderr) so they don't
    confuse scripts that pipe stderr to /dev/null while parsing
    stdout. Tests asserting against output should look at
    ``result.stdout``.
    """
    typer.secho(message, fg=typer.colors.GREEN, bold=True)


def info(message: str) -> None:
    """Print a user-facing informational message to stdout in cyan.

    Use for hints / status notes that aren't success per se —
    "Reboot to apply", "Operation queued — uuid X", etc. The cyan
    is distinct enough from green-success that a script-reading
    user can tell at a glance whether something completed or is
    waiting on an action.
    """
    typer.secho(message, fg=typer.colors.CYAN)


def warning(message: str) -> None:
    """Print a user-facing warning to stderr in yellow.

    Goes to stderr (not stdout) so scripts capturing stdout for
    parsing don't see the warning in their data stream. Use for
    "the call succeeded but something looks off" cases —
    deprecation hints, surprising upstream behaviour, edge
    conditions that don't actually fail.
    """
    typer.secho(f"Warning: {message}", err=True, fg=typer.colors.YELLOW)


def print_table(
    title: str,
    rows: list[dict[str, Any]],
    *,
    columns: list[str] | None = None,
    fmt: OutputFormat = OutputFormat.TABLE,
) -> None:
    """Render ``rows`` (list of homogenous dicts) per the chosen format.

    ``columns`` overrides the column order. When omitted, columns are
    taken from the first row's key order — Python 3.7+ preserves
    dict insertion order so this is deterministic per the calling
    code.

    Empty ``rows`` is acceptable; we render an empty table with the
    headers (or print ``[]`` for JSON).
    """
    if fmt is OutputFormat.JSON:
        sys.stdout.write(json.dumps(rows, indent=2, ensure_ascii=False))
        sys.stdout.write("\n")
        return

    if fmt is OutputFormat.YAML:
        _yaml_dump(rows)
        return

    # Table rendering.
    if columns is None:
        columns = list(rows[0].keys()) if rows else []
    table = Table(title=title, header_style="bold cyan", show_lines=False)
    for col in columns:
        table.add_column(col)
    for row in rows:
        table.add_row(*[_render_cell(row.get(col)) for col in columns])
    _stdout.print(table)


def print_dict(
    title: str,
    data: dict[str, Any],
    *,
    fmt: OutputFormat = OutputFormat.TABLE,
) -> None:
    """Render a single resource (dict) — two-column ``Field / Value``
    table by default."""
    if fmt is OutputFormat.JSON:
        sys.stdout.write(json.dumps(data, indent=2, ensure_ascii=False))
        sys.stdout.write("\n")
        return

    if fmt is OutputFormat.YAML:
        _yaml_dump(data)
        return

    table = Table(title=title, header_style="bold cyan", show_header=True)
    table.add_column("Field")
    table.add_column("Value")
    for k, v in data.items():
        table.add_row(str(k), _render_cell(v))
    _stdout.print(table)


def _render_cell(value: Any) -> str:
    """Format a single cell for table rendering.

    Uses ASCII glyphs (``-``, ``yes`` / ``no``) rather than fancy
    Unicode (``—`` / ``✓`` / ``✗``) because Windows legacy consoles
    default to cp1252 and crash on the latter — Phase 1.6's smoke
    learned this the hard way and we want CLI output to read on
    every terminal, not just modern UTF-8 ones.
    """
    if value is None:
        return "[dim]-[/]"
    if isinstance(value, bool):
        return "yes" if value else "no"
    return str(value)


def _yaml_dump(data: Any) -> None:
    """Defer-import PyYAML so installs that never touch YAML output
    don't pay the dependency cost.

    Phase 2.7 wired pyyaml as the optional ``[yaml]`` extra. Calling
    this function on an install that didn't pull in that extra
    surfaces a clear ImportError telling the user how to upgrade or
    pick a different format.
    """
    try:
        import yaml
    except ImportError as exc:
        raise RuntimeError(
            "YAML output requires the optional `pyyaml` dependency. "
            "Install with: pip install impreza-cli[yaml]"
        ) from exc
    sys.stdout.write(yaml.safe_dump(data, sort_keys=False, allow_unicode=True))
