"""Shared helpers used across CLI commands.

Originally introduced as ``_vps_helpers.py`` in Phase 3.4 to break
a circular-import between ``commands/vps.py`` and the per-backend
sub-resource modules. Renamed to ``_helpers.py`` in Phase 4.3
because the helpers are not VPS-specific — every command that
catches :class:`~impreza.exceptions.ApiError` benefits from
routing through :func:`exit_on_api_error`.

Conventions kept consistent across every CLI command:

* :func:`exit_on_api_error` — the canonical mapping of
  :class:`~impreza.exceptions.ApiError` to a single stderr line plus
  ``raise typer.Exit(1)``. The 5 commands that grew their own local
  copy during Phase 2 (account, catalog, domain, invoice, key)
  consolidated onto this single import in 4.3.
* :func:`resolve_vps_or_exit` — bound-model lookup that maps
  ``ResourceNotFound`` and "service exists but is not a VPS"
  (:class:`InvalidRequest`) to the same friendly stderr lines the
  3.2 power verbs introduced. VPS-only by design.
* :func:`wait_for_operation` — the dotted-progress poll loop used
  by reinstall / migrate (3.3) and by snapshots-rollback / backups-
  create / backups-restore (3.4 onwards). Reimplements
  ``op.wait()`` because the SDK helper is silent — for long-running
  operations, silence reads as a hang.
"""

from __future__ import annotations

import time
from typing import Any

import typer
from impreza import Operation, Vps
from impreza.exceptions import ApiError, InvalidRequest, ResourceNotFound

from ..output import error


def exit_on_api_error(exc: ApiError) -> None:
    """Render a one-line error from an SDK :class:`ApiError` to
    stderr and ``raise typer.Exit(1)``.

    Includes the upstream ``code`` and request id when present so
    bug reports can be triaged without re-running with ``--debug``.
    Never returns — the ``raise`` is part of the contract.
    """
    parts = [exc.message]
    if exc.code:
        parts.append(f"(code={exc.code})")
    if exc.request_id:
        parts.append(f"[request_id={exc.request_id}]")
    error(" ".join(parts))
    raise typer.Exit(code=1)


def resolve_vps_or_exit(client: Any, service_id: int) -> Vps:
    """Look up a VPS by id, mapping the standard SDK errors to the
    same friendly stderr lines the 3.2 power verbs introduced.

    Returns the bound :class:`~impreza.Vps` model (``VpsProxmox`` or
    ``VpsCloud``). Callers can then dispatch to the right SDK method
    without caring which backend they're on.
    """
    try:
        return client.vps.get(service_id)
    except ResourceNotFound:
        error(f"VPS service {service_id} not found on this account.")
        raise typer.Exit(code=1) from None
    except InvalidRequest as exc:
        # "Service exists but isn't a VPS" — the SDK message is
        # already friendly, pass it through.
        exit_on_api_error(exc)
        raise  # unreachable — exit_on_api_error always raises


def wait_for_operation(
    op: Operation,
    *,
    label: str,
    timeout: int,
    poll_interval: float = 2.0,
) -> None:
    """Block on an :class:`Operation` future, printing one dot per
    poll cycle so the user gets visible feedback during long
    upstream queue runs.

    Reimplements ``op.wait()`` rather than calling it because the
    SDK helper is silent — for an operation that can run minutes
    (reinstall, migrate, backup create/restore, snapshot rollback),
    silence reads as a hang. Maps the SDK's
    :class:`OperationFailed` / :class:`OperationTimeout` paths to
    clean CLI errors with a remediation hint pointing at
    ``--timeout``.
    """
    typer.echo(f"{label} (operation {op.uuid[:12]}) ", nl=False)
    elapsed = 0.0
    while not op.is_done():
        if elapsed >= timeout:
            typer.echo()
            error(
                f"Operation {op.uuid} did not finish within "
                f"{timeout}s (last status: {op.status!r}). "
                "Re-run with a larger --timeout to wait longer."
            )
            raise typer.Exit(code=1)
        typer.echo(".", nl=False)
        time.sleep(poll_interval)
        elapsed += poll_interval
        try:
            op.refresh()
        except ApiError as exc:
            typer.echo()
            exit_on_api_error(exc)
    typer.echo(" done.")
    if op.is_failure():
        msg = f"Operation {op.uuid} ended in {op.status!r}"
        if op.error:
            msg += f" — {op.error}"
        error(msg + ".")
        raise typer.Exit(code=1)


__all__ = [
    "exit_on_api_error",
    "resolve_vps_or_exit",
    "wait_for_operation",
]
