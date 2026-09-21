"""``impreza config`` — configuration as code for custom deployments (P4).

Three commands over the reviewed config surface:

* ``impreza config export <deployment> [--out FILE]`` — write the versioned
  document (secrets as references) to stdout or a file;
* ``impreza config apply <deployment> --file FILE [--yes]`` — prepare a
  reviewed apply from the document, show the server-computed diff, and only
  queue it after an explicit confirmation. The apply carries the exact
  review digest, so what you confirmed is byte-for-byte what runs;
* ``impreza config plan <plan-id>`` — read a saved review's status or
  replay its receipt.

The document is meant for version control: export it, commit it, edit it,
apply it. Secret values never appear in the document — applying resolves
the references server-side.
"""

from __future__ import annotations

import os
import re
import stat
from pathlib import Path

import typer
from impreza.exceptions import ApiError

from ..output import error, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context
from ._helpers import exit_on_api_error

app = typer.Typer(
    name="config",
    help="Export, review and apply versioned application configuration.",
    no_args_is_help=True,
)


def _terminal_text(value: str) -> str:
    # Escape terminal control characters and bidi overrides in server text.
    return "".join(
        ch
        if ch in "\n\t"
        or (
            ord(ch) >= 32
            and not 127 <= ord(ch) <= 159
            and not 0x202A <= ord(ch) <= 0x202E
            and not 0x2066 <= ord(ch) <= 0x2069
        )
        else f"\\u{ord(ch):04x}"
        for ch in value
    )


@app.command("export")
def export(
    typer_ctx: typer.Context,
    deployment_id: str = typer.Argument(..., help="Custom deployment id (dpl_…)."),
    out: Path | None = typer.Option(
        None,
        "--out",
        "-o",
        help="Write the document to this file instead of stdout.",
    ),
) -> None:
    """Export the versioned config document (secrets as references)."""
    client = make_client_or_exit(from_typer_context(typer_ctx))
    try:
        payload = client.config.export(deployment_id)
    except ApiError as exc:
        exit_on_api_error(exc)
    except ValueError as exc:
        error(str(exc))
        raise typer.Exit(1) from exc
    document_text = payload.get("document_text")
    if not isinstance(document_text, str) or not document_text.strip():
        error("The export carried no document text.")
        raise typer.Exit(1)
    if out is not None:
        out.write_text(document_text, encoding="utf-8")
        success(
            f"Exported config for {payload.get('deployment_id', deployment_id)} "
            f"(schema {payload.get('schema', '?')}) to {out}."
        )
        return
    typer.echo(document_text, nl=False)


@app.command("apply")
def apply(
    typer_ctx: typer.Context,
    deployment_id: str = typer.Argument(..., help="Custom deployment id (dpl_…)."),
    file: Path = typer.Option(
        ...,
        "--file",
        "-f",
        help="Config document to apply (as written by `impreza config export`).",
        exists=True,
        readable=True,
        dir_okay=False,
    ),
    yes: bool = typer.Option(
        False,
        "--yes",
        "-y",
        help="Apply without the interactive confirmation prompt.",
    ),
) -> None:
    """Prepare a reviewed apply from a document, show its diff, confirm, apply."""
    try:
        if not file.is_file():
            raise ValueError("Config input must be a regular file.")
        # Nonblocking open also prevents a raced replacement with a FIFO from
        # hanging automation. Validate the opened descriptor, then bound reads.
        fd = os.open(file, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_BINARY", 0))
        with os.fdopen(fd, "rb") as stream:
            if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                raise ValueError("Config input must be a regular file.")
            raw = stream.read(65537)
        if not raw or len(raw) > 65536:
            raise ValueError("Config document must contain 1 to 65536 UTF-8 bytes.")
        document = raw.decode("utf-8")
    except (OSError, ValueError) as exc:
        error("Cannot read config document: " + str(exc))
        raise typer.Exit(1) from exc
    client = make_client_or_exit(from_typer_context(typer_ctx))
    try:
        review = client.config.prepare(deployment_id, document)
    except ApiError as exc:
        exit_on_api_error(exc)
    except ValueError as exc:
        error(str(exc))
        raise typer.Exit(1) from exc
    plan_id = review.get("plan_id")
    review_digest = review.get("review_digest")
    changes = review.get("changes")
    diff_text = review.get("diff_text")
    if (
        not isinstance(plan_id, str)
        or re.fullmatch(r"cplan_[a-f0-9]{24}", plan_id) is None
        or not isinstance(review_digest, str)
        or re.fullmatch(r"[a-f0-9]{64}", review_digest) is None
        or review.get("deployment_id") != deployment_id
        or review.get("operation") != "config_apply"
        or not isinstance(changes, list)
        or not all(isinstance(item, str) for item in changes)
        or not isinstance(diff_text, str)
        or review.get("no_changes") is not (changes == [])
    ):
        error(
            "The review response is incomplete or does not match this deployment; "
            "nothing was applied."
        )
        raise typer.Exit(1)

    if isinstance(changes, (list, dict)) and changes:
        typer.secho("Planned changes:", fg=typer.colors.YELLOW, bold=True)
        if isinstance(changes, dict):
            for key, value in changes.items():
                typer.echo(f"  {key}: {value}")
        else:
            for line in changes:
                typer.echo(_terminal_text(f"  {line}"))
    else:
        typer.echo("No changes detected — the apply is idempotent for this document.")
    if isinstance(diff_text, str) and diff_text.strip():
        typer.echo("")
        typer.secho("Diff:", fg=typer.colors.YELLOW, bold=True)
        typer.echo(_terminal_text(diff_text.rstrip()))

    typer.echo("")
    confirm_or_exit(f"Apply this configuration to {deployment_id}.", yes=yes)
    try:
        result = client.config.apply(plan_id, review_digest)
    except ApiError as exc:
        exit_on_api_error(exc)
    except ValueError as exc:
        error(str(exc))
        raise typer.Exit(1) from exc
    receipt = result.get("receipt")
    if not isinstance(receipt, dict):
        receipt = result
    command_id = result.get("command_id") or receipt.get("command_id")
    replayed = bool(result.get("replayed"))
    if replayed:
        success(f"Plan {plan_id} was already accepted — replayed its receipt.")
    else:
        success(f"Configuration applied (plan {plan_id}).")
    if isinstance(command_id, str) and command_id:
        typer.echo(_terminal_text(f"Deploy command: {command_id}"))
    if receipt.get("no_changes") is True:
        typer.echo("No deployment was queued because the configuration already matches.")
    else:
        typer.echo("The change is queued; watch the deployment's status or the command above.")


@app.command("plan")
def plan(
    typer_ctx: typer.Context,
    plan_id: str = typer.Argument(..., help="Config plan id (cplan_…)."),
) -> None:
    """Read one saved config review: status, digest and receipt."""
    client = make_client_or_exit(from_typer_context(typer_ctx))
    try:
        payload = client.config.get_plan(plan_id)
    except ApiError as exc:
        exit_on_api_error(exc)
    except ValueError as exc:
        error(str(exc))
        raise typer.Exit(1) from exc
    status = payload.get("status", "unknown")
    typer.echo(_terminal_text(f"Plan {plan_id}: {status}"))
    digest = payload.get("review_digest")
    if isinstance(digest, str) and digest:
        typer.echo(_terminal_text(f"Review digest: {digest}"))
    receipt = payload.get("receipt")
    if isinstance(receipt, dict):
        typer.echo(f"Receipt: {receipt}")
