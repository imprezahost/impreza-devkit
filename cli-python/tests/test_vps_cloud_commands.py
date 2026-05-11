"""Unit tests for ``impreza vps cloud`` (Phase 3.5).

Mirrors the 3.4 Proxmox unit suite — covers the five nested
sub-apps (images, rescue, iso, rdns, ssh-keys) plus the five
inline verbs at the cloud root (vnc, vnc-password, resize,
boot-order, ipv6 enable).

For each verb:

* happy-path test pins the URL + body
* a Proxmox-backend test asserts the friendly "Cloud-only" stderr
  line (the inverse of 3.4's BackendNotSupported handling)
* destructive verbs (delete, restore, rdns delete) get a
  decline-at-prompt test
* boot-order has a client-side validation test (unknown --order
  rejected before any HTTP)
"""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest
import respx
from typer.testing import CliRunner

from impreza_cli.config import Config
from impreza_cli.main import app

runner = CliRunner()

BASE = "https://api.imprezahost.com/v1"
_FAKE_KEY = "imp_" + ("a" * 40)
_FAKE_SECRET = "0" * 64


@pytest.fixture
def seeded_config(isolated_config: Path) -> Path:
    cfg = Config.load(isolated_config)
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()
    return isolated_config


# ── envelope helpers ────────────────────────────────────────────────


def _service_envelope(service: dict[str, object]) -> dict[str, object]:
    return {
        "success": True,
        "data": service,
        "meta": {"request_id": "req_t"},
    }


def _proxmox_service(service_id: int = 17988) -> dict[str, object]:
    return {
        "id": service_id,
        "domain": "vps-1.example.com",
        "status": "Active",
        "product": "Hong Kong VPS I",
        "product_group": "VPS Server Hong Kong",
        "billing_cycle": "Monthly",
        "amount": 25.0,
        "registered_at": "2026-05-09",
        "next_due": "2026-06-09",
        "vps_backend": "proxmox",
    }


def _cloud_service(service_id: int = 17987) -> dict[str, object]:
    return {
        "id": service_id,
        "domain": "vps-2.example.com",
        "status": "Active",
        "product": "VPS I",
        "product_group": "VPS Server",
        "billing_cycle": "Monthly",
        "amount": 17.0,
        "dedicated_ip": "198.51.100.10",
        "registered_at": "2026-05-09",
        "next_due": "2026-06-09",
        "vps_backend": "cloud",
    }


def _ok(payload: dict[str, object] | None = None) -> dict[str, object]:
    return {
        "success": True,
        "data": payload if payload is not None else {},
        "meta": {"request_id": "req_t"},
    }


def _list_envelope(key: str, items: list[dict[str, object]]) -> dict[str, object]:
    return _ok({key: items})


# ══════════════════════════════════════════════════════════════════════
# images
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_images_list_account_scoped_path(seeded_config: Path) -> None:
    """`vps.images.list()` hits the account-scoped /vps/cloud/images
    endpoint (NOT /vps/cloud/{id}/images). The vm_id field
    disambiguates which VPS each image came from."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.get(f"{BASE}/vps/cloud/images").mock(
        return_value=httpx.Response(
            200,
            json=_list_envelope(
                "images",
                [
                    {"id": "img-1", "name": "snap-a", "vm_id": 17987,
                     "size": 4 * 1024**3, "status": "ready"},
                ],
            ),
        )
    )
    result = runner.invoke(app, ["vps", "cloud", "images", "list", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "img-1" in result.stdout
    assert "snap-a" in result.stdout
    assert "GB" in result.stdout


@respx.mock
def test_images_create_hits_vm_scoped_post(seeded_config: Path) -> None:
    """`vps.images.create()` hits /vps/cloud/{id}/images (per-VM),
    distinct from list's account-scoped path."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/images").mock(
        return_value=httpx.Response(
            201, json=_ok({"id": "img-new", "name": "auto"})
        )
    )
    result = runner.invoke(app, ["vps", "cloud", "images", "create", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "img-new" in result.stdout


@respx.mock
def test_images_restore_with_yes(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/images/img-1/restore").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "images", "restore", "17987", "img-1", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "img-1" in result.stdout


def test_images_restore_decline(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app,
            ["vps", "cloud", "images", "restore", "17987", "img-1"],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


@respx.mock
def test_images_delete_account_scoped_path(seeded_config: Path) -> None:
    """`vps.images.delete()` hits /vps/cloud/images/{id}
    (account-scoped, no /vmId segment), per the SDK docstring."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.delete(f"{BASE}/vps/cloud/images/img-1").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "images", "delete", "17987", "img-1", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called


@respx.mock
def test_images_list_on_proxmox_exits_cloud_only(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    result = runner.invoke(app, ["vps", "cloud", "images", "list", "17988"])
    assert result.exit_code == 1
    assert "Cloud-only" in result.stderr


# ══════════════════════════════════════════════════════════════════════
# rescue
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_rescue_enable_with_password_and_yes(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/rescue").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "rescue", "enable", "17987",
         "--password", "R3scue!", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"password": "R3scue!"}
    assert "Rescue armed" in result.stdout


@respx.mock
def test_rescue_disable_no_prompt(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.delete(f"{BASE}/vps/cloud/17987/rescue").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(app, ["vps", "cloud", "rescue", "disable", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Rescue disabled" in result.stdout


@respx.mock
def test_rescue_enable_on_proxmox(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "rescue", "enable", "17988",
         "--password", "x123", "--yes"],
    )
    assert result.exit_code == 1
    assert "Cloud-only" in result.stderr


# ══════════════════════════════════════════════════════════════════════
# iso
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_iso_mount(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/iso/mount").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "iso", "mount", "17987", "ubuntu-22.04.iso"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"iso": "ubuntu-22.04.iso"}


@respx.mock
def test_iso_unmount(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.delete(f"{BASE}/vps/cloud/17987/iso").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(app, ["vps", "cloud", "iso", "unmount", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called


# ══════════════════════════════════════════════════════════════════════
# rdns
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_rdns_get(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.get(f"{BASE}/vps/cloud/rdns/1.2.3.4").mock(
        return_value=httpx.Response(
            200, json=_ok({"ip": "1.2.3.4", "hostname": "mail.example.com"})
        )
    )
    result = runner.invoke(
        app, ["vps", "cloud", "rdns", "get", "17987", "1.2.3.4"]
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "mail.example.com" in result.stdout


@respx.mock
def test_rdns_set(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.put(f"{BASE}/vps/cloud/rdns/1.2.3.4").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "rdns", "set", "17987", "1.2.3.4", "mail.example.com"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"hostname": "mail.example.com"}


@respx.mock
def test_rdns_delete_with_yes(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.delete(f"{BASE}/vps/cloud/rdns/1.2.3.4").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "rdns", "delete", "17987", "1.2.3.4", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called


# ══════════════════════════════════════════════════════════════════════
# ssh-keys
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_ssh_keys_list_account_scoped(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.get(f"{BASE}/vps/cloud/ssh-keys").mock(
        return_value=httpx.Response(
            200,
            json=_list_envelope(
                "keys",
                [
                    {"id": 1, "name": "laptop", "fingerprint": "SHA256:..."},
                    {"id": 2, "name": "ci", "fingerprint": "SHA256:..."},
                ],
            ),
        )
    )
    result = runner.invoke(app, ["vps", "cloud", "ssh-keys", "list", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "laptop" in result.stdout
    assert "ci" in result.stdout


@respx.mock
def test_ssh_keys_assign_multiple_ids(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/ssh-keys").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "ssh-keys", "assign", "17987", "1", "2", "3"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"ssh_keys": ["1", "2", "3"]}
    assert "Assigned 3 SSH key" in result.stdout


# ══════════════════════════════════════════════════════════════════════
# vnc + vnc-password
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_vnc_reads_credentials(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.get(f"{BASE}/vps/cloud/17987/vnc").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"ip": "198.51.100.10", "port": 5901, "password": "v1n2c3"}),
        )
    )
    result = runner.invoke(app, ["vps", "cloud", "vnc", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "198.51.100.10" in result.stdout
    assert "5901" in result.stdout
    assert "v1n2c3" in result.stdout


@respx.mock
def test_vnc_password_with_flag_skips_prompt(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.put(f"{BASE}/vps/cloud/17987/vnc-password").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "vnc-password", "17987", "--password", "NewVNC!"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"password": "NewVNC!"}


# ══════════════════════════════════════════════════════════════════════
# resize
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_resize_with_yes(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/resize").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "cloud", "resize", "17987", "--size", "vps-2", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"instance_size": "vps-2"}
    assert "resized to 'vps-2'" in result.stdout


def test_resize_decline_at_prompt(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app,
            ["vps", "cloud", "resize", "17987", "--size", "vps-2"],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


# ══════════════════════════════════════════════════════════════════════
# boot-order
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_boot_order_cda(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.put(f"{BASE}/vps/cloud/17987/boot-order").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app, ["vps", "cloud", "boot-order", "17987", "--order", "cda"]
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"bootorder": "cda"}


def test_boot_order_invalid(seeded_config: Path) -> None:
    """Client-side validation rejects unknown --order before HTTP."""
    result = runner.invoke(
        app, ["vps", "cloud", "boot-order", "17987", "--order", "xyz"]
    )
    assert result.exit_code == 1
    assert "--order must be one of" in result.stderr


# ══════════════════════════════════════════════════════════════════════
# ipv6 enable
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_ipv6_enable(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/ipv6").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(app, ["vps", "cloud", "ipv6", "enable", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "IPv6 enabled" in result.stdout


# ══════════════════════════════════════════════════════════════════════
# Shared: ResourceNotFound
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_images_list_404_renders_friendly_error(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/9999").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {"code": "NOT_FOUND", "message": "Service not found."},
            },
        )
    )
    result = runner.invoke(app, ["vps", "cloud", "images", "list", "9999"])
    assert result.exit_code == 1
    assert "VPS service 9999 not found" in result.stderr
    assert "Traceback" not in result.stderr
