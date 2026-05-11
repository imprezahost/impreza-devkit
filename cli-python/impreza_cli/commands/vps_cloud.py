"""``impreza vps cloud`` sub-command surface — Phase 3.5.

Cloud sub-resources mounted on the main ``vps`` Typer
app as the ``cloud`` namespace. Mirror of the 3.4 Proxmox layout.

Five nested sub-apps + five inline verbs at the cloud root:

* ``vps cloud images`` — ``list / create / restore / delete``
  Account-scoped on the wire (the Cloud image catalog is
  per-account, not per-VM); ``create`` snapshots the bound VM and
  ``restore`` brings an image back onto the bound VM.

* ``vps cloud rescue`` — ``enable / disable``
  Reboot the VM after ``enable`` to actually enter rescue mode.

* ``vps cloud iso`` — ``mount / unmount``

* ``vps cloud rdns`` — ``get / set / delete``
  Account-scoped on the API side (``/vps/cloud/rdns/{ip}``); the
  SDK exposes it via the bound VPS for ergonomics. ``<ip>`` selects
  the record within the account.

* ``vps cloud ssh-keys`` — ``list / assign``
  ``list`` returns every key on the Cloud account; ``assign``
  attaches one or more existing keys to the bound VPS.

Inline verbs (no sub-app):

* ``vps cloud vnc <id>`` — read the VNC client triple (host, port,
  password).
* ``vps cloud vnc-password <id>`` — rotate the VNC password
  (prompted hidden+confirmed by default).
* ``vps cloud resize <id> --size SIZE`` — change the Cloud backend
  instance size. Reboot required to apply.
* ``vps cloud boot-order <id> --order ORDER`` — set boot order;
  client-side validated against ``{"cda", "dca"}``.
* ``vps cloud ipv6 <id> enable`` — enable IPv6 on the VM.

Every verb maps ``BackendNotSupported`` (raised by the SDK
sub-resource property or the ``_require_backend("cloud", ...)``
check on a Proxmox VPS) to a friendly "This VPS is on the Proxmox
backend — <op> is Cloud-only." stderr line, mirroring the 3.4
Proxmox-only pattern in reverse.

Catalog reads (``catalog vps-cloud-sizes`` / ``vps-cloud-locations``)
deferred again — the SDK still hasn't shipped wrappers as of 3.5
kickoff. Pushed to a future fase; add a small SDK task before
re-attempting.
"""

from __future__ import annotations

from typing import Any

import typer
from impreza.exceptions import ApiError, BackendNotSupported

from ..output import OutputFormat, error, info, print_dict, print_table, success
from ..sdk import make_client_or_exit
from ..state import confirm_or_exit, from_typer_context, resolve_output
from ._helpers import exit_on_api_error, resolve_vps_or_exit

# ── parent Typer app + nested sub-apps ──────────────────────────────


app = typer.Typer(
    name="cloud",
    help="Cloud-only sub-resources: images, rescue, iso, rdns, ssh-keys, etc.",
    no_args_is_help=True,
)

images_app = typer.Typer(
    name="images",
    help="Manage saved Cloud VM images (account-scoped on the wire).",
    no_args_is_help=True,
)
rescue_app = typer.Typer(
    name="rescue",
    help="Enable / disable rescue mode on a Cloud VPS.",
    no_args_is_help=True,
)
iso_app = typer.Typer(
    name="iso",
    help="Mount / unmount an ISO on a Cloud VPS.",
    no_args_is_help=True,
)
rdns_app = typer.Typer(
    name="rdns",
    help="Reverse-DNS records for Cloud account IPs.",
    no_args_is_help=True,
)
ssh_keys_app = typer.Typer(
    name="ssh-keys",
    help="List account-level SSH keys and assign to a Cloud VPS.",
    no_args_is_help=True,
)
ipv6_app = typer.Typer(
    name="ipv6",
    help="IPv6 operations on a Cloud VPS.",
    no_args_is_help=True,
)

app.add_typer(images_app)
app.add_typer(rescue_app)
app.add_typer(iso_app)
app.add_typer(rdns_app)
app.add_typer(ssh_keys_app)
app.add_typer(ipv6_app)


# ── helpers ─────────────────────────────────────────────────────────


def _cloud_only_exit(op: str) -> None:
    """Print the standard "Cloud-only" stderr line and exit 1.
    Mirrors :func:`commands.vps_proxmox._proxmox_only_exit` — every
    sub-resource property on a Proxmox VPS raises
    :class:`BackendNotSupported`.
    """
    error(f"This VPS is on the Proxmox backend — {op} is Cloud-only.")
    raise typer.Exit(code=1) from None


def _format_bytes(value: int | None) -> str:
    """Human-readable byte count for image sizes. ``None`` → ``-``."""
    if value is None:
        return "-"
    mb = 1024 * 1024
    gb = 1024 * mb
    if value >= gb:
        return f"{value / gb:.2f} GB"
    if value >= mb:
        return f"{value / mb:.0f} MB"
    return f"{value} B"


_VALID_BOOT_ORDER = {"cda", "dca"}


# ══════════════════════════════════════════════════════════════════════
# Images
# ══════════════════════════════════════════════════════════════════════


@images_app.command("list")
def images_list(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List saved images on the Cloud account.

    Wraps ``vps.images.list()``. The Cloud image catalog is
    account-scoped — this returns every image, regardless of which
    Cloud VPS originally created it. Use the ``vm_id`` column to
    spot which VPS each image came from.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            images = vps.images.list()
        except BackendNotSupported:
            _cloud_only_exit("images")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not images:
        typer.echo("No images saved on this account.")
        return
    if fmt is OutputFormat.TABLE:
        rows = [
            {
                "id": img.id,
                "name": img.name or "",
                "vm_id": img.vm_id if img.vm_id is not None else "",
                "size": _format_bytes(img.size),
                "status": img.status or "",
                "created_at": img.created_at or "",
            }
            for img in images
        ]
    else:
        rows = [
            {
                "id": img.id,
                "name": img.name,
                "vm_id": img.vm_id,
                "size": img.size,
                "status": img.status,
                "created_at": img.created_at,
            }
            for img in images
        ]
    print_table(
        f"Cloud images ({len(rows)})",
        rows,
        columns=["id", "name", "vm_id", "size", "status", "created_at"],
        fmt=fmt,
    )


@images_app.command("create")
def images_create(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
) -> None:
    """Snapshot the bound VM's current state into a saved image.

    Wraps ``vps.images.create()``. Synchronous — returns the
    :class:`Image` model. The new image is added to the account-
    level catalog and can be restored to any Cloud VPS the account
    owns via ``vps cloud images restore``.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            image = vps.images.create()
        except BackendNotSupported:
            _cloud_only_exit("images")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(
        f"Image created from VPS {service_id}: id={image.id!r}"
        + (f", name={image.name!r}" if image.name else "")
    )


@images_app.command("restore")
def images_restore(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    image_id: str = typer.Argument(..., help="Image id from `vps cloud images list`."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the data-loss confirmation prompt."
    ),
) -> None:
    """Restore an image onto the bound Cloud VPS. **Destructive** —
    overwrites the current disk with the image contents.

    Wraps ``vps.images.restore(image_id)``. Synchronous on the
    Cloud backend side.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Restoring image {image_id!r} onto VPS {service_id} overwrites "
        "the current disk. Any changes made since the image was created "
        "will be lost.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.images.restore(image_id)
        except BackendNotSupported:
            _cloud_only_exit("images")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Image {image_id!r} restored onto VPS {service_id}.")


@images_app.command("delete")
def images_delete(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(
        ...,
        help=(
            "Service id of any Cloud VPS on the account (the image "
            "catalog is account-scoped; this id is only used to resolve "
            "the backend and confirm the account)."
        ),
    ),
    image_id: str = typer.Argument(..., help="Image id to delete."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the deletion confirmation prompt."
    ),
) -> None:
    """Delete an image from the Cloud account. **Irreversible.**

    Wraps ``vps.images.delete(image_id)``. The API path
    ``DELETE /vps/cloud/images/{id}`` is account-scoped — passing
    any Cloud VPS id on the account is enough to reach it.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Deleting image {image_id!r} from the account is irreversible.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.images.delete(image_id)
        except BackendNotSupported:
            _cloud_only_exit("images")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Image {image_id!r} deleted from the account.")


# ══════════════════════════════════════════════════════════════════════
# Rescue
# ══════════════════════════════════════════════════════════════════════


@rescue_app.command("enable")
def rescue_enable(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    password: str = typer.Option(
        ...,
        "--password", "-p",
        prompt="Rescue root password",
        hide_input=True,
        confirmation_prompt=True,
        help=(
            "Root password for the rescue environment. Prompts hidden "
            "(with confirmation) when omitted. Passing on the command "
            "line puts the password in shell history — prefer the prompt."
        ),
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the reboot-required confirmation prompt."
    ),
) -> None:
    """Enable rescue mode on a Cloud VPS. A reboot is required to
    actually enter rescue — the upstream marks the VM for rescue on
    the next boot.

    Wraps ``vps.rescue.enable(password=...)``.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Enabling rescue on VPS {service_id} marks the VM for rescue. "
        "You must reboot for it to take effect (`impreza vps reboot`).",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.rescue.enable(password=password)
        except BackendNotSupported:
            _cloud_only_exit("rescue")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    info(
        f"Rescue armed on VPS {service_id}. Reboot to enter rescue mode."
    )


@rescue_app.command("disable")
def rescue_disable(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
) -> None:
    """Disarm rescue mode. The VM will boot normally on next reboot.

    Wraps ``vps.rescue.disable()``. No confirmation prompt —
    disabling rescue is a recovery action with no data impact.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.rescue.disable()
        except BackendNotSupported:
            _cloud_only_exit("rescue")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Rescue disabled on VPS {service_id}.")


# ══════════════════════════════════════════════════════════════════════
# ISO
# ══════════════════════════════════════════════════════════════════════


@iso_app.command("mount")
def iso_mount(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    iso: str = typer.Argument(
        ...,
        help=(
            "ISO identifier (typically the file name as registered with "
            "Cloud backend). Available ISOs vary per location — check the "
            "Impreza Account control panel for the catalog."
        ),
    ),
) -> None:
    """Mount an ISO on a Cloud VPS. Reboot required for the VM to
    actually boot from it.

    Wraps ``vps.iso.mount(iso)``.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.iso.mount(iso)
        except BackendNotSupported:
            _cloud_only_exit("iso")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    info(f"ISO {iso!r} mounted on VPS {service_id}. Reboot to boot from it.")


@iso_app.command("unmount")
def iso_unmount(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
) -> None:
    """Unmount the ISO. Takes effect on next reboot.

    Wraps ``vps.iso.unmount()``.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.iso.unmount()
        except BackendNotSupported:
            _cloud_only_exit("iso")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"ISO unmounted on VPS {service_id}.")


# ══════════════════════════════════════════════════════════════════════
# rDNS
# ══════════════════════════════════════════════════════════════════════


@rdns_app.command("get")
def rdns_get(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(
        ...,
        help=(
            "Service id of any Cloud VPS on the account (rDNS is "
            "account-scoped; this id is only used to confirm the account)."
        ),
    ),
    ip: str = typer.Argument(..., help="IP address whose rDNS to read."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Read the rDNS record for an IP.

    Wraps ``vps.rdns.get(ip)``. The API path is account-scoped
    (``GET /vps/cloud/rdns/{ip}``).
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            result = vps.rdns.get(ip)
        except BackendNotSupported:
            _cloud_only_exit("rdns")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not result:
        typer.echo(f"No rDNS record for {ip}.")
        return
    print_dict(f"rDNS — {ip}", {str(k): v for k, v in result.items()}, fmt=fmt)


@rdns_app.command("set")
def rdns_set(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(
        ...,
        help="Service id of any Cloud VPS on the account (see `rdns get`).",
    ),
    ip: str = typer.Argument(..., help="IP address to set rDNS on."),
    hostname: str = typer.Argument(..., help="Hostname to point the rDNS PTR at."),
) -> None:
    """Set the rDNS PTR record for an IP.

    Wraps ``vps.rdns.set(ip, hostname)``. Some upstreams reject
    hostnames that don't resolve forward to the IP — those return
    a 4xx and the SDK surfaces it as :class:`InvalidRequest`.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.rdns.set(ip, hostname)
        except BackendNotSupported:
            _cloud_only_exit("rdns")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"rDNS for {ip} set to {hostname!r}.")


@rdns_app.command("delete")
def rdns_delete(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(
        ...,
        help="Service id of any Cloud VPS on the account (see `rdns get`).",
    ),
    ip: str = typer.Argument(..., help="IP address whose rDNS to clear."),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the deletion confirmation prompt."
    ),
) -> None:
    """Delete the rDNS record for an IP.

    Wraps ``vps.rdns.delete(ip)``. After deletion, reverse-DNS
    lookups on the IP return whatever default the upstream
    configures.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Removing rDNS for {ip} reverts reverse-DNS lookups to the "
        "upstream default. Mail servers and rate-limiters relying on "
        "the PTR may behave differently.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            vps.rdns.delete(ip)
        except BackendNotSupported:
            _cloud_only_exit("rdns")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"rDNS removed for {ip}.")


# ══════════════════════════════════════════════════════════════════════
# SSH keys
# ══════════════════════════════════════════════════════════════════════


@ssh_keys_app.command("list")
def ssh_keys_list(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(
        ...,
        help=(
            "Service id of any Cloud VPS on the account (SSH key "
            "catalog is account-scoped; this id is only used to confirm "
            "the account)."
        ),
    ),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """List SSH keys registered on the Cloud account.

    Wraps ``vps.ssh_keys.list()``. The catalog is account-scoped —
    every key returned is available to assign to any Cloud VPS on
    the account via ``vps cloud ssh-keys assign``.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            keys = vps.ssh_keys.list()
        except BackendNotSupported:
            _cloud_only_exit("ssh-keys")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    if not keys:
        typer.echo("No SSH keys registered on this account.")
        return
    rows = [
        {"id": k.id, "name": k.name, "fingerprint": k.fingerprint or ""}
        for k in keys
    ]
    print_table(
        f"SSH keys ({len(rows)})",
        rows,
        columns=["id", "name", "fingerprint"],
        fmt=fmt,
    )


@ssh_keys_app.command("assign")
def ssh_keys_assign(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    key_ids: list[str] = typer.Argument(
        ...,
        help=(
            "One or more SSH key ids from `vps cloud ssh-keys list`. "
            "Pass each id as a separate argument: `... assign 17987 1 2 3`."
        ),
    ),
) -> None:
    """Assign one or more account-level SSH keys to a Cloud VPS.

    Wraps ``vps.ssh_keys.assign([...])``. The assigned keys are
    injected into ``~/.ssh/authorized_keys`` for the root user on
    the next provisioning event (rebuild, reinstall, rescue).
    Existing instances may need a reboot for the keys to be picked
    up — depends on the Cloud image template.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps = resolve_vps_or_exit(client, service_id)
        try:
            # The SDK accepts list[str | int]; pass strings verbatim and
            # let the upstream coerce. Typer would lose the type either
            # way (CLI args are always strings).
            vps.ssh_keys.assign(list(key_ids))
        except BackendNotSupported:
            _cloud_only_exit("ssh-keys")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(
        f"Assigned {len(key_ids)} SSH key(s) to VPS {service_id}: "
        + ", ".join(key_ids)
    )


# ══════════════════════════════════════════════════════════════════════
# VNC (inline at cloud root)
# ══════════════════════════════════════════════════════════════════════


@app.command("vnc")
def vnc(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    output: OutputFormat | None = typer.Option(
        None, "--output", "-o",
        help="Output format. Overrides the global --output flag.",
        case_sensitive=False,
    ),
) -> None:
    """Read VNC client credentials (host, port, password).

    Wraps ``vps.vnc()``. Connect with a desktop VNC client
    (TigerVNC, RealVNC, etc.). The credentials are short-lived;
    re-fetch when you reconnect after extended idle.
    """
    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)
    with make_client_or_exit(state) as client:
        vps_obj = resolve_vps_or_exit(client, service_id)
        try:
            creds = vps_obj.vnc()
        except BackendNotSupported:
            _cloud_only_exit("vnc")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return

    data: dict[str, Any] = {
        "ip": creds.ip,
        "port": creds.port,
        "password": creds.password,
    }
    print_dict(f"VNC — VPS {service_id}", data, fmt=fmt)


@app.command("vnc-password")
def vnc_password(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    password: str = typer.Option(
        ...,
        "--password", "-p",
        prompt="New VNC password",
        hide_input=True,
        confirmation_prompt=True,
        help=(
            "New VNC password. Prompts hidden (with confirmation) when "
            "omitted. Passing on the command line puts the password in "
            "shell history — prefer the prompt."
        ),
    ),
) -> None:
    """Rotate the VNC password.

    Wraps ``vps.vnc_password(password)``. The next ``vps cloud vnc``
    call returns the new password.
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps_obj = resolve_vps_or_exit(client, service_id)
        try:
            vps_obj.vnc_password(password)
        except BackendNotSupported:
            _cloud_only_exit("vnc-password")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"VNC password rotated for VPS {service_id}.")


# ══════════════════════════════════════════════════════════════════════
# Resize (inline)
# ══════════════════════════════════════════════════════════════════════


@app.command("resize")
def resize(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    size: str = typer.Option(
        ...,
        "--size", "-s",
        help=(
            "New Cloud instance size identifier. Available sizes "
            "depend on the VPS's location — once 3.5+ ships the SDK "
            "wrapper, `impreza catalog vps-cloud-sizes` will list them. "
            "Until then, your Impreza Account is authoritative."
        ),
    ),
    yes: bool = typer.Option(
        False, "--yes", "-y", help="Skip the resize confirmation prompt."
    ),
) -> None:
    """Resize the Cloud VPS to a new Cloud instance size.
    **Reboot required** for the resize to apply on the guest.

    Wraps ``vps.resize(instance_size=size)``. Billing adjusts on
    the next invoice — Cloud charges the difference between
    the old and new size pro-rated for the remaining billing cycle.
    """
    state = from_typer_context(typer_ctx)
    confirm_or_exit(
        f"Resizing VPS {service_id} to size {size!r} requires a reboot "
        "to take effect and adjusts billing on the next invoice.",
        yes=yes,
    )
    with make_client_or_exit(state) as client:
        vps_obj = resolve_vps_or_exit(client, service_id)
        try:
            vps_obj.resize(instance_size=size)
        except BackendNotSupported:
            _cloud_only_exit("resize")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    info(
        f"VPS {service_id} resized to {size!r}. Reboot for the change to apply."
    )


# ══════════════════════════════════════════════════════════════════════
# Boot order (inline)
# ══════════════════════════════════════════════════════════════════════


@app.command("boot-order")
def boot_order(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
    order: str = typer.Option(
        ...,
        "--order", "-o",
        help=(
            "Boot order: 'cda' (disk → CD-ROM → network) or 'dca' "
            "(CD-ROM → disk → network). Other strings are rejected "
            "client-side."
        ),
    ),
) -> None:
    """Set the BIOS boot order on a Cloud VPS.

    Wraps ``vps.boot_order(order)``. ``--order`` is validated
    client-side against ``{"cda", "dca"}`` so unknown values exit 1
    before any HTTP call.
    """
    if order not in _VALID_BOOT_ORDER:
        error(
            f"--order must be one of {sorted(_VALID_BOOT_ORDER)!r}, "
            f"got: {order!r}"
        )
        raise typer.Exit(code=1)

    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps_obj = resolve_vps_or_exit(client, service_id)
        try:
            # mypy needs the Literal here; the runtime check above
            # already narrows it, but a cast keeps strict happy.
            from typing import Literal, cast
            vps_obj.boot_order(cast(Literal["cda", "dca"], order))
        except BackendNotSupported:
            _cloud_only_exit("boot-order")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"Boot order on VPS {service_id} set to {order!r}.")


# ══════════════════════════════════════════════════════════════════════
# IPv6 (sub-app with `enable` verb)
# ══════════════════════════════════════════════════════════════════════


@ipv6_app.command("enable")
def ipv6_enable(
    typer_ctx: typer.Context,
    service_id: int = typer.Argument(..., help="Service id (Cloud VPS)."),
) -> None:
    """Enable IPv6 on the VM.

    Wraps ``vps.ipv6_enable()``. Reboot may be required for the
    interface to come up inside the guest (depends on the
    Cloud image template).
    """
    state = from_typer_context(typer_ctx)
    with make_client_or_exit(state) as client:
        vps_obj = resolve_vps_or_exit(client, service_id)
        try:
            vps_obj.ipv6_enable()
        except BackendNotSupported:
            _cloud_only_exit("ipv6")
            return
        except ApiError as exc:
            exit_on_api_error(exc)
            return
    success(f"IPv6 enabled on VPS {service_id}.")
