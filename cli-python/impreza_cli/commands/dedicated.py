"""``impreza dedicated`` subcommand surface — Phase 8.

Twenty verbs over the :class:`~impreza.DedicatedResource` shipped in
the SDK alongside this CLI. The public ``/dedicated/*`` namespace is
vendor-agnostic: operations are gated by per-service capabilities,
and capability-gated endpoints return ``NOT_SUPPORTED`` when the
underlying service doesn't advertise the feature. Always inspect
``dedicated capabilities <id>`` first when scripting.

Discovery & state:
* ``list`` / ``show <id>`` / ``capabilities <id>`` / ``status <id>``
* ``ips <id>`` — IPs with current PTR
* ``os-images <id>`` — OS images available for reinstall

Power:
* ``start <id>`` / ``shutdown <id>`` / ``reboot <id>``

rDNS:
* ``set-rdns <id> --ip --hostname``
* ``reset-rdns <id>``

KVM / IPMI:
* ``kvm <id>`` — current access info
* ``enable-kvm <id>`` — the SDK / server inject the caller's public
  IP as the session-binding IP when the service needs it.
* ``disable-kvm <id>``

Firewall (requires ``firewall`` capability):
* ``firewall <id>`` / ``ddos-logs <id>``
* ``set-firewall <id> --ip --state --sensitivity``

Bandwidth (requires ``bandwidth`` capability):
* ``bandwidth <id> --type --scale``

VPN credentials (requires ``vpn`` capability):
* ``vpn <id>``

Reinstall (destructive — wipes ALL data):
* ``reinstall <id> --os-id --password --confirm [--yes]``
    Requires both the SDK's ``confirm=True`` flag and the
    ``X-Impreza-Confirm: WIPE`` header (the SDK injects the header
    when ``confirm=True``). The CLI also prompts on a TTY unless
    ``--yes`` is passed.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError

from ..output import OutputFormat, error, info, print_dict, print_table, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context, resolve_output
from ._helpers import exit_on_api_error

app = typer.Typer(
    name="dedicated",
    help="Manage dedicated servers. Operations gated by per-service capabilities.",
    no_args_is_help=True,
)


# ── helpers ─────────────────────────────────────────────────────────


def _emit(value: Any, *, ctx: typer.Context) -> None:
    """Render a heterogeneous payload (dict, list[dict], or scalar).

    The /dedicated/* payloads vary by service — there's no single
    typed projection that fits every endpoint. Tables turn into flat
    key=value dumps; JSON / YAML pass the structure through.
    """
    state = from_typer_context(ctx)
    fmt = resolve_output(state, override=None)
    if fmt is OutputFormat.TABLE and isinstance(value, dict):
        print_dict(value)
    elif fmt is OutputFormat.TABLE and isinstance(value, list) and value and isinstance(value[0], dict):
        # Use the first row's keys as the column order so the table
        # is stable across runs.
        columns = list(value[0].keys())
        print_table(columns, value)
    else:
        print_dict(value) if isinstance(value, dict) else typer.echo(_serialize(value, fmt))


def _serialize(value: Any, fmt: OutputFormat) -> str:
    """Format a non-dict value for JSON / YAML output."""
    import json

    if fmt is OutputFormat.YAML:
        try:
            import yaml  # type: ignore[import-not-found]

            return yaml.safe_dump(value, sort_keys=False).rstrip()
        except ImportError:  # pragma: no cover
            return json.dumps(value, indent=2, ensure_ascii=False)
    return json.dumps(value, indent=2, ensure_ascii=False)


# ── discovery ──────────────────────────────────────────────────────


@app.command("list")
def cmd_list(ctx: typer.Context) -> None:
    """List every dedicated server on the account."""
    client = make_client_or_exit(ctx)
    try:
        items = client.dedicated.list()
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    if not items:
        info("No dedicated servers on this account.")
        return
    fmt = resolve_output(from_typer_context(ctx), override=None)
    if fmt is OutputFormat.TABLE:
        columns = ["service_id", "domain", "ip", "status", "capabilities"]
        rows = [
            {
                "service_id": item.get("service_id"),
                "domain": item.get("domain", ""),
                "ip": item.get("ip", ""),
                "status": item.get("status", ""),
                "capabilities": ",".join(item.get("capabilities") or []),
            }
            for item in items
        ]
        print_table(columns, rows)
    else:
        _emit(items, ctx=ctx)


@app.command("show")
def cmd_show(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Show full details for a dedicated server."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.info(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("capabilities")
def cmd_capabilities(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Show the capability list advertised by a dedicated server.

    Call this before any capability-gated sub-command — firewall,
    bandwidth, vpn, kvm, reinstall, power — so you don't waste a
    round-trip on a NOT_SUPPORTED response.
    """
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.capabilities(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("status")
def cmd_status(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Show current power / provisioning state."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.status(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("ips")
def cmd_ips(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """List the IPs assigned to the server with current PTR."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.ips(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("os-images")
def cmd_os_images(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """List OS images available for reinstall."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.os_images(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


# ── power ──────────────────────────────────────────────────────────


@app.command("start")
def cmd_start(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Power on a dedicated server."""
    client = make_client_or_exit(ctx)
    try:
        client.dedicated.start(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    success(f"Dedicated server {service_id} start signal sent.")


@app.command("shutdown")
def cmd_shutdown(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Graceful shutdown."""
    client = make_client_or_exit(ctx)
    try:
        client.dedicated.shutdown(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    success(f"Dedicated server {service_id} shutdown signal sent.")


@app.command("reboot")
def cmd_reboot(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Reboot."""
    client = make_client_or_exit(ctx)
    try:
        client.dedicated.reboot(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    success(f"Dedicated server {service_id} reboot signal sent.")


# ── rDNS ───────────────────────────────────────────────────────────


@app.command("set-rdns")
def cmd_set_rdns(
    ctx: typer.Context,
    service_id: int = typer.Argument(...),
    ip: str = typer.Option(..., "--ip", help="IP whose PTR you want to set."),
    hostname: str = typer.Option(..., "--hostname", help="New PTR value."),
) -> None:
    """Set the PTR for a single IP on a dedicated server.

    Applied synchronously on most services. On services without an
    automated rDNS path the response is ``{status: queued, ...}`` and
    an operator on our side completes it within a few hours.
    """
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.set_rdns(service_id, ip, hostname)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("reset-rdns")
def cmd_reset_rdns(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Reset every PTR back to the Impreza default (impreza.host pattern)."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.reset_rdns(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


# ── KVM ────────────────────────────────────────────────────────────


@app.command("kvm")
def cmd_kvm(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Show current KVM / IPMI access info."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.kvm(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("enable-kvm")
def cmd_enable_kvm(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Open a KVM/IPMI session.

    The caller's public IP is injected as the session-binding IP
    automatically when the service needs it — nothing for you to
    pass.
    """
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.enable_kvm(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("disable-kvm")
def cmd_disable_kvm(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Close the active KVM/IPMI session."""
    client = make_client_or_exit(ctx)
    try:
        client.dedicated.disable_kvm(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    success(f"KVM session for service {service_id} disabled.")


# ── Firewall (capability-gated) ────────────────────────────────────


@app.command("firewall")
def cmd_firewall(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Show DDoS firewall state. Requires the ``firewall`` capability."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.firewall(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("ddos-logs")
def cmd_ddos_logs(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Show DDoS attack logs. Requires the ``firewall`` capability."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.ddos_logs(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


@app.command("set-firewall")
def cmd_set_firewall(
    ctx: typer.Context,
    service_id: int = typer.Argument(...),
    ip: str = typer.Option(..., "--ip", help="IP to update."),
    state: str | None = typer.Option(
        None,
        "--state",
        help="always_on | redirect_on_attack (omit to leave unchanged).",
    ),
    sensitivity: str | None = typer.Option(
        None,
        "--sensitivity",
        help="low | normal | medium | high (omit to leave unchanged).",
    ),
) -> None:
    """Update DDoS firewall state/sensitivity for an IP. Requires the ``firewall`` capability."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.set_firewall(
            service_id, ip=ip, state=state, sensitivity=sensitivity
        )
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


# ── Bandwidth (capability-gated) ───────────────────────────────────


@app.command("bandwidth")
def cmd_bandwidth(
    ctx: typer.Context,
    service_id: int = typer.Argument(...),
    type: str = typer.Option(
        "port_bits",
        "--type",
        help="port_bits | port_upkts | port_percent | port_errors | port_pktsize | port_discards",
    ),
    scale: str = typer.Option("month", "--scale", help="day | week | month"),
) -> None:
    """Bandwidth graph (PNG base64). Requires the ``bandwidth`` capability."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.bandwidth(service_id, type=type, scale=scale)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


# ── VPN (capability-gated) ─────────────────────────────────────────


@app.command("vpn")
def cmd_vpn(ctx: typer.Context, service_id: int = typer.Argument(...)) -> None:
    """Show rotating VPN credentials. Requires the ``vpn`` capability."""
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.vpn(service_id)
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)


# ── Reinstall (destructive) ────────────────────────────────────────


@app.command("reinstall")
def cmd_reinstall(
    ctx: typer.Context,
    service_id: int = typer.Argument(...),
    os_id: str = typer.Option(..., "--os-id", help="OS id from `dedicated os-images <id>`."),
    password: str = typer.Option(
        ..., "--password", help="New root password (min 8 chars).", prompt=True, hide_input=True
    ),
    os_label: str | None = typer.Option(
        None, "--os-label", help="Optional OS label — auto-resolved from os-id if absent."
    ),
    confirm: bool = typer.Option(
        False,
        "--confirm",
        help="Acknowledge that ALL DATA will be wiped.",
    ),
    yes: bool = typer.Option(False, "--yes", "-y", help="Skip the interactive prompt."),
) -> None:
    """Reinstall the OS. Destructive — wipes ALL data.

    On services that support a synchronous reinstall path, the result
    returns immediately and includes the new root password. On services
    where the reinstall has to be applied manually, the response is
    ``{status: queued, message: ...}`` and our team executes the
    reinstall within a few hours.

    Both ``--confirm`` AND the prompt (or ``--yes``) must pass. The
    required ``X-Impreza-Confirm: WIPE`` header is injected by the SDK
    automatically.
    """
    if not confirm:
        error("Reinstall is destructive — pass --confirm to acknowledge data loss.")
        raise typer.Exit(code=1)
    confirm_or_exit(
        f"Reinstall service {service_id} to os-id={os_id}? ALL DATA WILL BE LOST.",
        yes=yes,
    )
    client = make_client_or_exit(ctx)
    try:
        data = client.dedicated.reinstall(
            service_id,
            os_id=os_id,
            password=password,
            confirm=True,
            os_label=os_label,
        )
    except ApiError as exc:
        exit_on_api_error(exc)
        return
    _emit(data, ctx=ctx)
