"""``impreza vps`` subcommand surface — Phases 2.5 + 3.2 + 3.3.

Read-only VPS commands over the smart-dispatch surface that ships
in 1.4b-i / 1.4b-ii:

* ``impreza vps list [--status STATUS] [--backend BACKEND]``
    Wraps ``c.vps.list()`` (which walks ``/account/services`` and
    keeps only entries with a non-null ``vps_backend``). Optional
    filters run client-side — the underlying SDK call doesn't take
    them, but the list is small enough that filtering after the
    fetch is fine.

* ``impreza vps show <id>``
    Wraps ``c.vps.get(id)`` and renders the resolved bound model's
    underlying ``Service`` snapshot.

* ``impreza vps status <id>``
    Wraps ``c.vps.get(id).status()`` and renders the
    :class:`~impreza.VpsStatus` snapshot. Table mode formats memory
    as MB / GB with units and uptime as a human-friendly duration;
    JSON emits the raw bytes / seconds for piping into jq.

Power verbs added in Phase 3.2:

* ``impreza vps start <id>`` / ``reboot <id>`` / ``shutdown <id>``
    Wrap ``c.vps.start/reboot/shutdown(id)`` from the smart-dispatch
    surface. ``shutdown`` is the safe graceful ACPI path — no
    confirmation by default. ``start`` and ``reboot`` are safe-ish
    boot-state transitions.

* ``impreza vps stop <id>``
    Force-stop (`/poweroff` on Cloud, `/stop` on Proxmox). May
    corrupt unwritten data on the guest — gated by
    ``confirm_or_exit``; pass ``--yes`` / ``-y`` to skip.

Management verbs added in Phase 3.3:

* ``impreza vps set-hostname <id> <hostname>`` (both backends)
* ``impreza vps set-password <id>`` — prompts hidden for the new
  password unless ``--password`` is passed (both backends)
* ``impreza vps reinstall <id> --template TPL`` — destructive,
  wipes the disk. ``--wait`` blocks on the Proxmox operation
  queue (synchronous on Cloud — ``--wait`` is a silent no-op
  there). Both backends.
* ``impreza vps migrate <id> --target TGT`` — Proxmox-only.
  Returns an Operation; ``--wait`` blocks until the upstream
  Proxmox queue settles.
* ``impreza vps cancel <id>`` — submits a cancellation request
  via ``AddCancelRequest`` for staff approval (the customer does
  not terminate the service directly; staff own the billing-cycle
  close). Defaults to ``--type "End of Billing Period"`` to
  discourage accidentally throwing away prepaid time. Both backends.

No ``suspend`` / ``unsuspend``: service suspension is a
billing-state operation owned by staff (overdue invoices auto-
resolve on payment, abuse holds resolve manually). The server
retired the underlying routes on 2026-05-11.

Backend-specific sub-resources (snapshots, backups, images,
rescue, etc.) get their own command groups in 3.4 / 3.5.
"""

from __future__ import annotations

from typing import Any, Literal

import typer
from impreza import Vps, VpsStatus
from impreza.exceptions import (
    ApiError,
    BackendNotSupported,
    InvalidRequest,
    ResourceNotFound,
)

from ..output import OutputFormat, error, info, print_dict, print_table, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context, resolve_output
from ._helpers import (
    exit_on_api_error as _exit_on_api_error,
)
from ._helpers import (
    resolve_vps_or_exit as _resolve_vps_or_exit,
)
from ._helpers import (
    wait_for_operation as _wait_for_operation,
)
from .vps_cloud import app as _cloud_app
from .vps_proxmox import app as _proxmox_app

app = typer.Typer(
    name="vps",
    help="Read VPS instances across both Proxmox and Cloud backends.",
    no_args_is_help=True,
)
app.add_typer(_proxmox_app, name="proxmox")
app.add_typer(_cloud_app, name="cloud")


# ── render helpers ──────────────────────────────────────────────────


_MB = 1024 * 1024
_GB = 1024 * _MB


def _format_bytes(value: int | None) -> str:
    """Render bytes as a human-friendly string for table output.
    ``None`` → ``-``. Values ≥ 1 GB switch units."""
    if value is None:
        return "-"
    if value >= _GB:
        return f"{value / _GB:.2f} GB"
    return f"{value / _MB:.0f} MB"


def _format_uptime(seconds: int | None) -> str:
    """Render uptime in seconds as ``Xd HHh MMm`` for table output."""
    if seconds is None:
        return "-"
    days, rem = divmod(seconds, 86400)
    hours, rem = divmod(rem, 3600)
    minutes, _ = divmod(rem, 60)
    if days > 0:
        return f"{days}d {hours:02d}h {minutes:02d}m"
    if hours > 0:
        return f"{hours}h {minutes:02d}m"
    return f"{minutes}m"


def _format_cpu_usage(value: float | None) -> str:
    """the Cloud backend doesn't report CPU usage; Proxmox returns it as a
    fraction (0.05 = 5%). Multiply and tag the unit."""
    if value is None:
        return "-"
    return f"{value * 100:.1f}%"


def _vps_to_row(vps: Vps) -> dict[str, Any]:
    """Lift the underlying Service snapshot into a flat row for
    `vps list` table mode."""
    s = vps.service
    return {
        "id": s.id,
        "domain": s.domain,
        "backend": vps.backend,
        "status": s.status,
        "product": s.product,
        "billing_cycle": s.billing_cycle,
        "amount": f"{s.amount:.2f}",
        "next_due": s.next_due,
    }


_LIST_COLUMNS = [
    "id",
    "domain",
    "backend",
    "status",
    "product",
    "billing_cycle",
    "amount",
    "next_due",
]


# ── vps list ────────────────────────────────────────────────────────


@app.command("list")
def list_vpss(
    typer_ctx: typer.Context,
    status: str | None = typer.Option(
        None,
        "--status",
        help=(
            "Filter by service status (Active, Pending, Suspended, "
            "Cancelled, Terminated, Fraud). Case-insensitive substring "
            "match — applied client-side after the API fetch."
        ),
    ),
    backend: str | None = typer.Option(
        None,
        "--backend",
        help="Filter by VPS backend: 'proxmox' or 'cloud'.",
        case_sensitive=False,
    ),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List every VPS the authenticated client owns across all backends.

    Wraps ``c.vps.list()``. The smart-dispatch list call returns one
    bound model per VPS service regardless of backend, so Proxmox and
    Cloud entries appear in the same table — the ``backend`` column
    disambiguates.

    Filters (``--status`` / ``--backend``) run client-side. The list
    is small enough on real accounts (single-digit to low-double-
    digit count) that the cost of pulling everything once and
    filtering in Python is negligible.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    if backend is not None and backend.lower() not in ("proxmox", "cloud"):
        error(f"--backend must be 'proxmox' or 'cloud', got: {backend!r}")
        raise typer.Exit(code=1)

    with make_client_or_exit(state) as client:
        try:
            vpss = client.vps.list()
        except ApiError as exc:
            _exit_on_api_error(exc)

    # Client-side filtering. Status match is case-insensitive substring
    # so users can pass "act" / "active" / "ACTIVE" interchangeably.
    if status is not None:
        needle = status.lower()
        vpss = [v for v in vpss if v.service.status.lower().find(needle) != -1]
    if backend is not None:
        wanted = backend.lower()
        vpss = [v for v in vpss if v.backend == wanted]

    if not vpss:
        if status or backend:
            filters = []
            if status:
                filters.append(f"status~={status!r}")
            if backend:
                filters.append(f"backend={backend!r}")
            typer.echo(f"No VPS services match the filter: {', '.join(filters)}.")
        else:
            typer.echo("No VPS services on this account.")
        return

    rows = [_vps_to_row(v) for v in vpss]
    title = f"VPS services ({len(rows)}"
    if status or backend:
        bits = []
        if status:
            bits.append(f"status~={status!r}")
        if backend:
            bits.append(f"backend={backend!r}")
        title += f" matching {', '.join(bits)}"
    title += ")"
    print_table(title, rows, columns=_LIST_COLUMNS, fmt=fmt)


# ── vps show ────────────────────────────────────────────────────────


@app.command("show")
def show(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(
        ...,
        help="Service id (the same id returned by `impreza account services`).",
    ),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show full detail for a single VPS service.

    Wraps ``c.vps.get(id)`` and renders the underlying Service
    snapshot — service id / domain / backend / status / product /
    billing cycle / amount / dedicated IP / next due date.

    For live power state / metrics, use ``impreza vps status <id>``.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            vps = client.vps.get(service_id)
        except ResourceNotFound:
            error(f"VPS service {service_id} not found on this account.")
            raise typer.Exit(code=1) from None
        except InvalidRequest as exc:
            # InvalidRequest includes "service exists but is not a VPS"
            # (NOT_A_VPS code) — pass the message through, the SDK
            # generates a friendly hint already.
            _exit_on_api_error(exc)
        except ApiError as exc:
            _exit_on_api_error(exc)

    s = vps.service
    data: dict[str, Any] = {
        "id": s.id,
        "domain": s.domain,
        "backend": vps.backend,
        "status": s.status,
        "product": s.product,
        "product_group": s.product_group,
        "billing_cycle": s.billing_cycle,
        "amount": (
            f"{s.amount:.2f}"
            if fmt is OutputFormat.TABLE
            else s.amount
        ),
        "dedicated_ip": s.dedicated_ip,
        "registered_at": s.registered_at,
        "next_due": s.next_due,
    }
    print_dict(f"VPS {service_id}", data, fmt=fmt)


# ── vps status ──────────────────────────────────────────────────────


@app.command("status")
def status_cmd(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Show live power state and runtime metrics.

    Wraps ``vps.status()`` after a single ``c.vps.get(id)`` lookup.
    Proxmox returns the full set (power state, CPU, memory, uptime);
    Cloud only populates ``power_state`` (CPU / memory / uptime stay
    None). Table mode formats memory as MB / GB with units and
    uptime as ``Xd HHh MMm``; JSON emits raw bytes / seconds for
    piping.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    with make_client_or_exit(state) as client:
        try:
            vps = client.vps.get(service_id)
            st: VpsStatus = vps.status()
        except ResourceNotFound:
            error(f"VPS service {service_id} not found on this account.")
            raise typer.Exit(code=1) from None
        except InvalidRequest as exc:
            _exit_on_api_error(exc)
        except ApiError as exc:
            _exit_on_api_error(exc)

    if fmt is OutputFormat.TABLE:
        # Compute memory percentage when both bytes are present.
        mem_pct = (
            f" ({st.memory_used / st.memory_total * 100:.1f}%)"
            if st.memory_used is not None and st.memory_total
            else ""
        )
        memory_str = (
            f"{_format_bytes(st.memory_used)} / "
            f"{_format_bytes(st.memory_total)}{mem_pct}"
        )
        data: dict[str, Any] = {
            "service_id": service_id,
            "backend": vps.backend,
            "power_state": st.power_state,
            "cpu_usage": _format_cpu_usage(st.cpu_usage),
            "memory": memory_str,
            "uptime": _format_uptime(st.uptime),
        }
    else:
        # JSON / YAML: raw values, no formatting — consumers can
        # transform as they wish.
        data = {
            "service_id": service_id,
            "backend": vps.backend,
            "power_state": st.power_state,
            "cpu_usage": st.cpu_usage,
            "memory_used": st.memory_used,
            "memory_total": st.memory_total,
            "uptime": st.uptime,
        }

    print_dict(f"VPS {service_id} — status", data, fmt=fmt)


# ── vps power (Phase 3.2) ───────────────────────────────────────────


# Power verbs all share the same plumbing: resolve service id →
# call the SDK dispatcher → print a tiny confirmation line. The
# only divergence is whether to confirm first (``stop`` only) and
# the human-readable verb in the success message.
_PowerAction = Literal["start", "stop", "reboot", "shutdown"]

_POWER_SUCCESS = {
    "start":    "Boot request sent for VPS {id}.",
    "stop":     "Force-stop request sent for VPS {id}.",
    "reboot":   "Reboot request sent for VPS {id}.",
    "shutdown": "Graceful shutdown request sent for VPS {id}.",
}


def _run_power(state: Any, service_id: int, action: _PowerAction) -> None:
    """Shared body for the four power verbs.

    Resolves the service id via the SDK dispatcher (which performs
    the backend lookup + URL normalisation), then prints a one-line
    confirmation. Upstream may take a few seconds to actually
    transition state — the SDK call returns when the API accepts
    the request. Polling for the new ``power_state`` is up to the
    caller (use ``impreza vps status <id>`` to check).
    """
    with make_client_or_exit(state) as client:
        try:
            getattr(client.vps, action)(service_id)
        except ResourceNotFound:
            error(f"VPS service {service_id} not found on this account.")
            raise typer.Exit(code=1) from None
        except InvalidRequest as exc:
            # Service exists but isn't a VPS (NOT_A_VPS code) — the
            # SDK message already explains. Pass through.
            _exit_on_api_error(exc)
        except ApiError as exc:
            _exit_on_api_error(exc)
    # Power request "sent", not "done" — the upstream may take a few
    # seconds to actually transition state, so info() (cyan) reads
    # more honestly than success() (green-bold).
    info(_POWER_SUCCESS[action].format(id=service_id))


@app.command("start")
def start(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
) -> None:
    """Boot a stopped VPS.

    Wraps ``c.vps.start(id)`` from the smart-dispatch surface.
    Proxmox hits ``POST /vps/proxmox/{id}/start``; Cloud hits
    ``POST /vps/cloud/{id}/boot`` (renamed upstream, hidden by the
    SDK dispatcher). Fire-and-forget on the HTTP wire — the API
    returns when the request is accepted, not when the guest is
    fully up. Re-check with ``impreza vps status <id>``.
    """
    state = from_typer_context(typer_ctx)
    _run_power(state, service_id, "start")


@app.command("reboot")
def reboot(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
) -> None:
    """Reboot a running VPS.

    Wraps ``c.vps.reboot(id)``. Both backends hit
    ``POST /vps/{backend}/{id}/reboot`` — no name divergence.
    Behaves as a guest-OS reboot on Proxmox and Cloud alike
    (graceful where supported, hard cycle if the guest doesn't
    respond in time, upstream-dependent).
    """
    state = from_typer_context(typer_ctx)
    _run_power(state, service_id, "reboot")


@app.command("shutdown")
def shutdown(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
) -> None:
    """Graceful ACPI shutdown.

    Wraps ``c.vps.shutdown(id)``. The guest OS receives a power-
    button signal and is expected to shut down cleanly. Safe to run
    without confirmation — no data loss when the guest cooperates.
    If the guest ignores the signal, use ``impreza vps stop`` (with
    ``--yes`` to acknowledge the corruption risk).
    """
    state = from_typer_context(typer_ctx)
    _run_power(state, service_id, "shutdown")


@app.command("stop")
def stop(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the corruption-risk confirmation prompt."
    ),
) -> None:
    """Force-stop (hard power-off). Equivalent to pulling the plug —
    unwritten guest data may be lost.

    Wraps ``c.vps.stop(id)``. Proxmox hits
    ``POST /vps/proxmox/{id}/stop``; Cloud hits the renamed
    ``POST /vps/cloud/{id}/poweroff``. Prefer ``shutdown`` whenever
    the guest is responsive — only reach for ``stop`` when the OS
    has hung and an ACPI signal won't get through.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Force-stopping VPS {service_id} cuts power immediately. "
        "Unwritten guest data may be lost — prefer `impreza vps "
        "shutdown` when the guest is responsive.",
        yes=yes,
    )
    _run_power(state, service_id, "stop")


# ── vps set-hostname / set-password (Phase 3.3) ─────────────────────


@app.command("set-hostname")
def set_hostname(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
    hostname: str = typer.Argument(..., help="New hostname for the VPS."),
) -> None:
    """Change the VPS hostname.

    Wraps ``vps.set_hostname(hostname)``. Both backends accept this
    surface. The change is applied immediately upstream — no
    confirmation prompt; rerunning with the previous value rolls
    back trivially.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = _resolve_vps_or_exit(client, service_id)
        try:
            vps.set_hostname(hostname)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"Hostname for VPS {service_id} set to {hostname!r}.")


@app.command("set-password")
def set_password(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
    password: str = typer.Option(
        ...,
        "--password", "-p",
        prompt="New root/admin password",
        hide_input=True,
        confirmation_prompt=True,
        help=(
            "New root/admin password. Prompts hidden (with "
            "confirmation) when omitted. Passing on the command line "
            "puts the password in shell history — prefer the prompt."
        ),
    ),
) -> None:
    """Reset the root/admin password.

    Wraps ``vps.set_password(password)``. Both backends supported.
    Active SSH sessions are not killed — the new password takes
    effect on next login. If the new password fails to apply
    (registrar-side complexity rules etc.), the upstream returns a
    400 and the SDK raises :class:`InvalidRequest`.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = _resolve_vps_or_exit(client, service_id)
        try:
            vps.set_password(password)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(f"Password updated for VPS {service_id}.")


# ── vps reinstall (Phase 3.3, with --wait) ──────────────────────────


@app.command("reinstall")
def reinstall(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
    template: str = typer.Option(
        ...,
        "--template", "-t",
        help=(
            "OS template identifier. Proxmox: see "
            "`vps.templates()` once 3.4 ships; for now use the value "
            "displayed in your Impreza Account (e.g. 'debian-12'). Cloud: "
            "image id from `vps cloud images list` (Phase 3.5)."
        ),
    ),
    password: str = typer.Option(
        ...,
        "--password", "-p",
        prompt="New root password for the reinstalled OS",
        hide_input=True,
        confirmation_prompt=True,
        help="Root/admin password for the freshly reinstalled OS.",
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the data-loss confirmation prompt."
    ),
    wait: bool = typer.Option(
        False,
        "--wait",
        help=(
            "Block until the Proxmox queue reports the reinstall as "
            "complete. No-op on Cloud (upstream is synchronous)."
        ),
    ),
    timeout: int = typer.Option(
        600,
        "--timeout",
        help="Max seconds to wait when --wait is set. Default 600 (10 min).",
    ),
) -> None:
    """Reinstall the operating system. **Destructive** — wipes the
    disk and any data on it.

    Wraps ``vps.reinstall(template=..., password=..., confirm=True)``.
    Behaviour diverges by backend:

    * **Proxmox**: returns an :class:`Operation` future. Without
      ``--wait`` the CLI prints the operation uuid and returns
      immediately; with ``--wait`` it polls until the upstream
      queue settles (or ``--timeout`` elapses).
    * **Cloud**: synchronous at the Cloud backend. The SDK
      returns ``None`` — the CLI prints a completion line and
      ``--wait`` is silently a no-op.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Reinstalling VPS {service_id} with template {template!r} "
        "wipes the disk. All data on this VPS will be lost.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = _resolve_vps_or_exit(client, service_id)
        try:
            op = vps.reinstall(template=template, password=password, confirm=True)
        except ApiError as exc:
            _exit_on_api_error(exc)
            return  # unreachable

        if op is None:
            # Cloud — synchronous from the SDK's perspective.
            success(
                f"Reinstall completed synchronously for VPS {service_id} "
                f"(template {template!r})."
            )
            return

        # Proxmox — Operation future. Stay inside the `with` so the
        # poll-loop's `op.refresh()` calls reuse the still-open HTTP
        # client.
        if not wait:
            info(
                f"Reinstall queued for VPS {service_id} (template {template!r}). "
                f"Operation uuid: {op.uuid}"
            )
            return
        _wait_for_operation(
            op,
            label=f"Reinstalling VPS {service_id}",
            timeout=timeout,
        )


# ── vps migrate (Phase 3.3, Proxmox-only, with --wait) ──────────────


@app.command("migrate")
def migrate(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Proxmox VPS)."),
    target: str = typer.Option(
        ...,
        "--target", "-t",
        help=(
            "Migration destination — a server_id or group_id string. "
            "Available targets come from `vps.locations()` once 3.4 "
            "ships; your Impreza Account also lists valid ids."
        ),
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the downtime confirmation prompt."
    ),
    wait: bool = typer.Option(
        False,
        "--wait",
        help="Block until the Proxmox queue reports the migration as complete.",
    ),
    timeout: int = typer.Option(
        1800,
        "--timeout",
        help="Max seconds to wait when --wait is set. Default 1800 (30 min).",
    ),
) -> None:
    """Migrate the VPS to a different physical host. **Proxmox-only.**

    Wraps ``vps.migrate(target=...)`` on the Proxmox bound model.
    Returns an :class:`Operation` future. Migration is a long
    upstream job (typically minutes, occasionally tens of minutes
    for large disks) and the VPS may experience downtime while the
    transfer runs.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Migrating VPS {service_id} to target {target!r} can cause "
        "downtime while the disk is transferred (typically several "
        "minutes for small VMs).",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = _resolve_vps_or_exit(client, service_id)
        try:
            op = vps.migrate(target=target)
        except BackendNotSupported:
            error(
                f"VPS {service_id} is on the Cloud backend — migrate is "
                "Proxmox-only."
            )
            raise typer.Exit(code=1) from None
        except ApiError as exc:
            _exit_on_api_error(exc)
            return  # unreachable

        if not wait:
            info(
                f"Migration queued for VPS {service_id} → {target!r}. "
                f"Operation uuid: {op.uuid}"
            )
            return
        _wait_for_operation(
            op,
            label=f"Migrating VPS {service_id} to {target!r}",
            timeout=timeout,
        )


# No `suspend` / `unsuspend` commands here. The server-side endpoints
# /vps/proxmox/{id}/suspend and /unsuspend were retired on 2026-05-11.
# Service suspension is a billing-state operation staff own — overdue
# invoices auto-resolve on payment, abuse holds resolve manually. To
# pause a guest, use `vps shutdown`; to wind down a service, use
# `vps cancel` (which submits an AddCancelRequest).


# ── vps cancel (Phase 3.3) ──────────────────────────────────────────


_CANCEL_TYPES = {"Immediate", "End of Billing Period"}


@app.command("cancel")
def cancel(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id."),
    cancel_type: str = typer.Option(
        "End of Billing Period",
        "--type", "-t",
        help=(
            "'Immediate' (terminate now, lose prepaid time) or "
            "'End of Billing Period' (keep until next due date). "
            "Default: 'End of Billing Period' so you don't accidentally "
            "throw away prepaid days."
        ),
    ),
    reason: str | None = typer.Option(
        None, "--reason", "-r", help="Optional cancellation reason for billing."
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the service-termination confirmation prompt."
    ),
) -> None:
    """Submit a cancellation request for the service. **Permanent.**

    Wraps ``vps.cancel(type=..., reason=...)``. Both backends. The
    type defaults to ``"End of Billing Period"`` — that way the user
    keeps the VPS up until the next renewal date instead of losing
    prepaid time. Pass ``--type Immediate`` to terminate right away
    (the prepaid balance is forfeit).
    """
    if cancel_type not in _CANCEL_TYPES:
        error(
            f"--type must be one of {sorted(_CANCEL_TYPES)!r}, "
            f"got: {cancel_type!r}"
        )
        raise typer.Exit(code=1)

    state = from_typer_context(typer_ctx)
    blast = (
        "immediately terminates the service (prepaid time is forfeit)"
        if cancel_type == "Immediate"
        else "schedules termination at the end of the current billing period"
    )
    confirm_or_exit(
        f"Cancelling VPS {service_id} ({cancel_type!r}) {blast}.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = _resolve_vps_or_exit(client, service_id)
        try:
            vps.cancel(type=cancel_type, reason=reason)
        except ApiError as exc:
            _exit_on_api_error(exc)
    success(
        f"Cancellation submitted for VPS {service_id} ({cancel_type})."
    )
