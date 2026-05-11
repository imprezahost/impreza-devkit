"""``impreza vps proxmox`` sub-command surface — Phase 3.4.

Proxmox-only sub-resources mounted on the main ``vps`` Typer app
as the ``proxmox`` namespace. Four groups:

* ``vps proxmox snapshots``
  - ``list <id>`` / ``create <id> <name>`` / ``delete <id> <name>``
    / ``rollback <id> <name>``  (rollback wraps Operation polling)

* ``vps proxmox backups``
  - ``list <id>`` / ``create <id>`` / ``restore <id> <backup-id>``
    / ``delete <id> <backup-id>``  (create + restore wrap Operation
    polling)

* ``vps proxmox backup-schedules``
  - ``list <id>`` / ``create <id> --dow ... --hour ... --minute ...``
    / ``delete <id> <schedule-id>``

* ``vps proxmox network``
  - ``reconfigure <id>`` — apply pending Proxmox network config

All commands here go through the ``Vps`` bound model
(``client.vps.get(id).snapshots`` / ``.backups`` / etc.) so a Cloud
VPS gets ``BackendNotSupported`` mapped to a friendly "Proxmox-only"
stderr line (mirroring the 3.3 pattern used for migrate / suspend).

The shared helpers (``resolve_vps_or_exit``, ``wait_for_operation``,
``exit_on_api_error``) live in ``commands/_helpers.py`` so this
module doesn't import from ``commands.vps`` and we avoid a cycle.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError, BackendNotSupported

from ..output import OutputFormat, error, info, print_dict, print_table, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context, resolve_output
from ._helpers import (
    exit_on_api_error,
    resolve_vps_or_exit,
    wait_for_operation,
)

# ── parent Typer app + nested sub-apps ──────────────────────────────


app = typer.Typer(
    name="proxmox",
    help="Proxmox-only sub-resources: snapshots, backups, schedules, network.",
    no_args_is_help=True,
)

snapshots_app = typer.Typer(
    name="snapshots",
    help="Manage Proxmox VM snapshots.",
    no_args_is_help=True,
)
backups_app = typer.Typer(
    name="backups",
    help="Manage Proxmox VM backups.",
    no_args_is_help=True,
)
schedules_app = typer.Typer(
    name="backup-schedules",
    help="Manage scheduled-backup jobs on a Proxmox VM.",
    no_args_is_help=True,
)
network_app = typer.Typer(
    name="network",
    help="Proxmox network operations (apply pending config).",
    no_args_is_help=True,
)

app.add_typer(snapshots_app)
app.add_typer(backups_app)
app.add_typer(schedules_app)
app.add_typer(network_app)


# ── helpers ─────────────────────────────────────────────────────────


def _proxmox_only_exit(op: str) -> None:
    """Print the standard "Proxmox-only" stderr line and exit 1.
    Called from every ``except BackendNotSupported:`` branch on the
    sub-resource verbs (mirroring the 3.3 migrate / suspend pattern).
    """
    error(f"This VPS is on the Cloud backend — {op} is Proxmox-only.")
    raise typer.Exit(code=1) from None


def _format_bytes(value: int | None) -> str:
    """Human-readable byte count for backup sizes. ``None`` → ``-``.
    Matches the table-mode renderer in :mod:`commands.vps`."""
    if value is None:
        return "-"
    mb = 1024 * 1024
    gb = 1024 * mb
    if value >= gb:
        return f"{value / gb:.2f} GB"
    if value >= mb:
        return f"{value / mb:.0f} MB"
    return f"{value} B"


# ══════════════════════════════════════════════════════════════════════
# Snapshots
# ══════════════════════════════════════════════════════════════════════


@snapshots_app.command("list")
def snapshots_list(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List Proxmox snapshots on the VPS.

    Wraps ``vps.snapshots.list()``. Renders ``name``, ``description``,
    and ``created_at`` in table mode; JSON / YAML emit the full
    :class:`~impreza.models.vps_extras.Snapshot` payload.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            snapshots = vps.snapshots.list()
        except BackendNotSupported:
            _proxmox_only_exit("snapshots")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not snapshots:
        typer.echo(f"No snapshots on VPS {service_id}.")
        return
    rows = [
        {
            "name": s.name,
            "description": s.description or "",
            "created_at": s.created_at or "",
        }
        for s in snapshots
    ]
    print_table(
        f"Snapshots — VPS {service_id} ({len(rows)})",
        rows,
        columns=["name", "description", "created_at"],
        fmt=fmt,
    )


@snapshots_app.command("create")
def snapshots_create(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    name: str = typer.Argument(
        ...,
        help=(
            "Snapshot name. Proxmox restricts to letters/digits/dashes/"
            "underscores; passing a name with other characters may be "
            "rejected upstream."
        ),
    ),
    description: str | None = typer.Option(
        None, "--description", "-d", help="Optional human-readable note."
    ),
) -> None:
    """Take a new Proxmox snapshot.

    Wraps ``vps.snapshots.create(name, description=...)``. Synchronous
    on the SDK side — returns the snapshot model directly.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            snapshot = vps.snapshots.create(name, description=description)
        except BackendNotSupported:
            _proxmox_only_exit("snapshots")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Snapshot {snapshot.name!r} created on VPS {service_id}.")


@snapshots_app.command("delete")
def snapshots_delete(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    name: str = typer.Argument(..., help="Snapshot name to delete."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the deletion confirmation prompt."
    ),
) -> None:
    """Delete a Proxmox snapshot. **Irreversible** — there's no
    undo; the saved VM state is freed at the storage layer.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Deleting snapshot {name!r} on VPS {service_id} is irreversible — "
        "the saved VM state is freed and cannot be recovered.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.snapshots.delete(name)
        except BackendNotSupported:
            _proxmox_only_exit("snapshots")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Snapshot {name!r} deleted from VPS {service_id}.")


@snapshots_app.command("rollback")
def snapshots_rollback(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    name: str = typer.Argument(..., help="Snapshot name to roll back to."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the data-loss confirmation prompt."
    ),
    wait: bool = typer.Option(
        False,
        "--wait",
        help="Block until the Proxmox queue reports the rollback as complete.",
    ),
    timeout: int = typer.Option(
        600,
        "--timeout",
        help="Max seconds to wait when --wait is set. Default 600 (10 min).",
    ),
) -> None:
    """Roll the VM back to a snapshot. **Destructive** — any disk
    changes since the snapshot was taken are lost. The VM is
    stopped during rollback.

    Returns an :class:`Operation` future. Without ``--wait`` the CLI
    surfaces the operation uuid and returns immediately; with
    ``--wait`` it polls until the upstream Proxmox queue settles
    (or ``--timeout`` elapses).
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Rolling back VPS {service_id} to snapshot {name!r} discards "
        "every disk change made after the snapshot was taken. The VM "
        "is stopped during rollback.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            op = vps.snapshots.rollback(name)
        except BackendNotSupported:
            _proxmox_only_exit("snapshots")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

        if not wait:
            info(
                f"Rollback queued for VPS {service_id} → snapshot {name!r}. "
                f"Operation uuid: {op.uuid}"
            )
            return
        wait_for_operation(
            op,
            label=f"Rolling back VPS {service_id} to {name!r}",
            timeout=timeout,
        )


# ══════════════════════════════════════════════════════════════════════
# Backups
# ══════════════════════════════════════════════════════════════════════


def _backup_row(b: Any) -> dict[str, Any]:
    """Lift a :class:`Backup` model into a flat row for table mode."""
    return {
        "id": b.id,
        "date": b.date or "",
        "size": _format_bytes(b.size),
        "mode": b.mode or "",
        "compress": b.compress or "",
        "protected": "yes" if b.protected else "",
        "notes": b.notes or "",
    }


@backups_app.command("list")
def backups_list(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List Proxmox backups on the VPS.

    Wraps ``vps.backups.list()``. Table mode renders ``size`` as
    MB/GB; JSON/YAML emit raw bytes for piping.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            backups = vps.backups.list()
        except BackendNotSupported:
            _proxmox_only_exit("backups")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not backups:
        typer.echo(f"No backups on VPS {service_id}.")
        return
    if fmt is OutputFormat.TABLE:
        rows = [_backup_row(b) for b in backups]
    else:
        rows = [
            {
                "id": b.id,
                "date": b.date,
                "size": b.size,
                "mode": b.mode,
                "compress": b.compress,
                "protected": b.protected,
                "notes": b.notes,
            }
            for b in backups
        ]
    print_table(
        f"Backups — VPS {service_id} ({len(rows)})",
        rows,
        columns=["id", "date", "size", "mode", "compress", "protected", "notes"],
        fmt=fmt,
    )


@backups_app.command("create")
def backups_create(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    wait: bool = typer.Option(
        False,
        "--wait",
        help="Block until the Proxmox queue reports the backup as complete.",
    ),
    timeout: int = typer.Option(
        1800,
        "--timeout",
        help="Max seconds to wait when --wait is set. Default 1800 (30 min).",
    ),
) -> None:
    """Trigger a new Proxmox backup.

    Wraps ``vps.backups.create()``. Returns an :class:`Operation`
    future — without ``--wait`` the CLI prints the uuid and exits;
    with ``--wait`` it polls (default 30-minute timeout since full
    disk dumps can run long on large VMs).

    Subject to the per-VM backup limit configured upstream — if you
    hit the limit, the API returns a 4xx and the SDK raises
    :class:`InvalidRequest`.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            op = vps.backups.create()
        except BackendNotSupported:
            _proxmox_only_exit("backups")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

        if not wait:
            info(
                f"Backup queued for VPS {service_id}. "
                f"Operation uuid: {op.uuid}"
            )
            return
        wait_for_operation(
            op,
            label=f"Creating backup for VPS {service_id}",
            timeout=timeout,
        )


@backups_app.command("restore")
def backups_restore(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    backup_id: str = typer.Argument(
        ...,
        help=(
            "Backup id from `vps proxmox backups list`. Accepts the "
            "registrar's id verbatim — typically a string but may be "
            "numeric depending on the storage backend."
        ),
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the data-loss confirmation prompt."
    ),
    wait: bool = typer.Option(
        False,
        "--wait",
        help="Block until the Proxmox queue reports the restore as complete.",
    ),
    timeout: int = typer.Option(
        1800,
        "--timeout",
        help="Max seconds to wait when --wait is set. Default 1800 (30 min).",
    ),
) -> None:
    """Restore a backup, overwriting the current disk. **Destructive.**

    Wraps ``vps.backups.restore(backup_id)``. The VM is stopped
    during restore, the disk is overwritten with the backup's
    contents, and the VM is left in whichever power state the
    backup recorded.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Restoring backup {backup_id!r} onto VPS {service_id} overwrites "
        "the current disk. Any changes made since the backup was taken "
        "will be lost.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            op = vps.backups.restore(backup_id)
        except BackendNotSupported:
            _proxmox_only_exit("backups")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

        if not wait:
            info(
                f"Restore queued for VPS {service_id} ← backup {backup_id!r}. "
                f"Operation uuid: {op.uuid}"
            )
            return
        wait_for_operation(
            op,
            label=f"Restoring VPS {service_id} from backup {backup_id!r}",
            timeout=timeout,
        )


@backups_app.command("delete")
def backups_delete(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    backup_id: str = typer.Argument(..., help="Backup id to delete."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the deletion confirmation prompt."
    ),
) -> None:
    """Delete a Proxmox backup. **Irreversible.**

    Wraps ``vps.backups.delete(backup_id)``. Protected backups
    return a 4xx from the upstream — the SDK surfaces that as
    :class:`InvalidRequest` with a clear message.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Deleting backup {backup_id!r} on VPS {service_id} is irreversible — "
        "the saved VM state is freed and cannot be recovered.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.backups.delete(backup_id)
        except BackendNotSupported:
            _proxmox_only_exit("backups")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Backup {backup_id!r} deleted from VPS {service_id}.")


# ══════════════════════════════════════════════════════════════════════
# Backup schedules
# ══════════════════════════════════════════════════════════════════════


_BACKUP_MODES = {"snapshot", "suspend", "stop"}
_BACKUP_COMPRESS = {"zstd", "lzo", "gzip", "none"}


@schedules_app.command("list")
def schedules_list(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List scheduled-backup jobs on the VPS.

    Wraps ``vps.backup_schedules.list()``. Day-of-week is rendered
    verbatim (the registrar accepts comma-separated tokens like
    ``mon,wed,fri``).
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            schedules = vps.backup_schedules.list()
        except BackendNotSupported:
            _proxmox_only_exit("backup-schedules")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not schedules:
        typer.echo(f"No backup schedules on VPS {service_id}.")
        return
    rows = [
        {
            "id": s.id,
            "dow": s.dow or "",
            "hour": s.hour if s.hour is not None else "",
            "minute": s.minute if s.minute is not None else "",
            "mode": s.mode or "",
            "compress": s.compress or "",
        }
        for s in schedules
    ]
    print_table(
        f"Backup schedules — VPS {service_id} ({len(rows)})",
        rows,
        columns=["id", "dow", "hour", "minute", "mode", "compress"],
        fmt=fmt,
    )


@schedules_app.command("create")
def schedules_create(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    dow: str = typer.Option(
        ...,
        "--dow",
        help=(
            "Day-of-week selector: comma-separated tokens "
            "(`mon,wed,fri`). Tokens are passed through to Proxmox "
            "verbatim — typo handling is upstream."
        ),
    ),
    hour: int = typer.Option(
        ..., "--hour", help="Hour of the day to run (0–23).", min=0, max=23
    ),
    minute: int = typer.Option(
        ..., "--minute", help="Minute past the hour (0–59).", min=0, max=59
    ),
    mode: str | None = typer.Option(
        None,
        "--mode",
        help=(
            "Backup mode: one of snapshot / suspend / stop. Default "
            "(upstream): snapshot. 'suspend' freezes the VM during "
            "backup; 'stop' powers it off."
        ),
    ),
    compress: str | None = typer.Option(
        None,
        "--compress",
        help=(
            "Compression algorithm: zstd / lzo / gzip / none. Default "
            "(upstream): zstd."
        ),
    ),
) -> None:
    """Create a scheduled backup job.

    Wraps ``vps.backup_schedules.create(...)``. ``--mode`` and
    ``--compress`` are validated client-side against the known
    Proxmox vocabularies; unknown values exit 1 before any HTTP
    call.
    """
    if mode is not None and mode not in _BACKUP_MODES:
        error(
            f"--mode must be one of {sorted(_BACKUP_MODES)!r}, "
            f"got: {mode!r}"
        )
        raise typer.Exit(code=1)
    if compress is not None and compress not in _BACKUP_COMPRESS:
        error(
            f"--compress must be one of {sorted(_BACKUP_COMPRESS)!r}, "
            f"got: {compress!r}"
        )
        raise typer.Exit(code=1)

    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            schedule = vps.backup_schedules.create(
                dow=dow, hour=hour, minute=minute, mode=mode, compress=compress
            )
        except BackendNotSupported:
            _proxmox_only_exit("backup-schedules")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(
        f"Backup schedule {schedule.id!r} created on VPS {service_id} "
        f"(dow={dow!r}, {hour:02d}:{minute:02d})."
    )


@schedules_app.command("delete")
def schedules_delete(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    schedule_id: str = typer.Argument(..., help="Schedule id to delete."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the deletion confirmation prompt."
    ),
) -> None:
    """Delete a scheduled backup job.

    Wraps ``vps.backup_schedules.delete(schedule_id)``. The job is
    removed from the upstream cron; in-flight runs are not
    interrupted (let them finish or use ``vps proxmox operations``
    to inspect once 3.4+ ships the operations command).
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Deleting backup schedule {schedule_id!r} on VPS {service_id} "
        "removes the recurring job — existing backups stay intact.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.backup_schedules.delete(schedule_id)
        except BackendNotSupported:
            _proxmox_only_exit("backup-schedules")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Backup schedule {schedule_id!r} deleted from VPS {service_id}.")


# ══════════════════════════════════════════════════════════════════════
# Network reconfigure
# ══════════════════════════════════════════════════════════════════════


@network_app.command("reconfigure")
def network_reconfigure(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Apply pending Proxmox network config.

    Wraps ``vps.network_reconfigure()``. Some changes (IP add/remove,
    DNS rotation) require either the Proxmox Guest Agent inside the
    VM **or** a reboot to take effect — this command pokes the
    agent. If the agent isn't installed, fall back to
    ``impreza vps reboot``.

    Returns whatever ack payload the registrar emits — typically
    ``{"applied": True}`` on success, or a more detailed payload
    when the registrar surfaces partial-apply state.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            result = vps.network_reconfigure()
        except BackendNotSupported:
            _proxmox_only_exit("network reconfigure")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not result:
        success(f"Network reconfigure requested on VPS {service_id}.")
        return
    print_dict(
        f"Network reconfigure — VPS {service_id}",
        {str(k): v for k, v in result.items()},
        fmt=fmt,
    )
