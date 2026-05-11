"""Unit tests for ``impreza vps`` commands.

Mocks ``c.vps.list()`` and ``c.vps.get()`` via respx — exact same
pattern as the catalog and domain command tests. Tests live
integration in test_phase_2_5_smoke.py.
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


def _services_envelope(services: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"services": services, "total": len(services)},
        "meta": {"request_id": "req_t"},
    }


def _service_envelope(service: dict[str, object]) -> dict[str, object]:
    return {
        "success": True,
        "data": service,
        "meta": {"request_id": "req_t"},
    }


def _proxmox_status_envelope(
    *,
    power_state: str = "running",
    cpu_usage: float = 0.05,
    memory_used: int = 1024 * 1024 * 1024,  # 1 GB
    memory_total: int = 2 * 1024 * 1024 * 1024,  # 2 GB
    uptime: int = 86400 * 3,  # 3 days
) -> dict[str, object]:
    """Proxmox status response (after our 1.7 normalisation fix —
    flat object with the OpenAPI VpsStatus fields)."""
    return {
        "success": True,
        "data": {
            "power_state": power_state,
            "cpu_usage": cpu_usage,
            "memory_used": memory_used,
            "memory_total": memory_total,
            "uptime": uptime,
        },
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


def _hosting_service(service_id: int = 15957) -> dict[str, object]:
    return {
        "id": service_id,
        "domain": "host.example.com",
        "status": "Active",
        "product": "USA Linux Hosting III",
        "product_group": "Linux Hosting",
        "billing_cycle": "Annually",
        "amount": 100.0,
        "registered_at": "2024-01-01",
        "next_due": "2027-01-01",
        "vps_backend": None,
    }


# ── vps list ────────────────────────────────────────────────────────


@respx.mock
def test_list_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_services_envelope(
                [_proxmox_service(), _cloud_service(), _hosting_service()]
            ),
        )
    )
    result = runner.invoke(app, ["vps", "list"])
    assert result.exit_code == 0, result.stderr
    # The hosting service (vps_backend is None) is filtered out by
    # the SDK's c.vps.list().
    assert "17988" in result.stdout
    assert "17987" in result.stdout
    assert "15957" not in result.stdout
    assert "proxmox" in result.stdout
    assert "cloud" in result.stdout


@respx.mock
def test_list_filter_by_backend_runs_client_side(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_services_envelope(
                [_proxmox_service(), _cloud_service()]
            ),
        )
    )
    result = runner.invoke(app, ["vps", "list", "--backend", "cloud"])
    assert result.exit_code == 0, result.stderr
    assert "17987" in result.stdout
    assert "17988" not in result.stdout


@respx.mock
def test_list_filter_by_status_substring(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_services_envelope(
                [
                    _proxmox_service(),
                    {**_cloud_service(), "status": "Suspended"},
                ]
            ),
        )
    )
    # Substring match: "act" matches "Active" but not "Suspended"
    result = runner.invoke(app, ["vps", "list", "--status", "act"])
    assert result.exit_code == 0
    assert "17988" in result.stdout
    assert "17987" not in result.stdout


def test_list_invalid_backend_exits_nonzero(seeded_config: Path) -> None:
    result = runner.invoke(app, ["vps", "list", "--backend", "kvm"])
    assert result.exit_code == 1
    assert "must be 'proxmox' or 'cloud'" in result.stderr


@respx.mock
def test_list_empty_default_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(200, json=_services_envelope([])),
    )
    result = runner.invoke(app, ["vps", "list"])
    assert result.exit_code == 0
    assert "No VPS services on this account" in result.stdout


@respx.mock
def test_list_empty_with_filter_mentions_filter(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200, json=_services_envelope([_proxmox_service()])
        )
    )
    result = runner.invoke(app, ["vps", "list", "--backend", "cloud"])
    assert result.exit_code == 0
    assert "No VPS services match the filter" in result.stdout
    assert "backend='cloud'" in result.stdout


@respx.mock
def test_list_json_output(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200, json=_services_envelope([_proxmox_service()])
        )
    )
    result = runner.invoke(app, ["vps", "list", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list) and len(parsed) == 1
    assert parsed[0]["id"] == 17988
    assert parsed[0]["backend"] == "proxmox"


# ── vps show ────────────────────────────────────────────────────────


@respx.mock
def test_show_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service())),
    )
    result = runner.invoke(app, ["vps", "show", "17988"])
    assert result.exit_code == 0, result.stderr
    assert "17988" in result.stdout
    assert "Hong Kong VPS I" in result.stdout
    assert "proxmox" in result.stdout


@respx.mock
def test_show_404_renders_friendly_error(seeded_config: Path) -> None:
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
    result = runner.invoke(app, ["vps", "show", "9999"])
    assert result.exit_code == 1
    assert "VPS service 9999 not found on this account" in result.stderr
    assert "Traceback" not in result.stderr


@respx.mock
def test_show_for_non_vps_service_renders_invalid_request(
    seeded_config: Path,
) -> None:
    """A service that exists but isn't a VPS (vps_backend=None)
    surfaces as InvalidRequest from the SDK; the CLI maps that
    through the standard ApiError formatter."""
    respx.get(f"{BASE}/account/services/15957").mock(
        return_value=httpx.Response(200, json=_service_envelope(_hosting_service()))
    )
    result = runner.invoke(app, ["vps", "show", "15957"])
    assert result.exit_code == 1
    assert "Traceback" not in result.stderr
    # The SDK's InvalidRequest hint mentions VPS / not a VPS
    stderr_lower = result.stderr.lower()
    assert "vps" in stderr_lower or "not_a_vps" in stderr_lower


@respx.mock
def test_show_json_emits_full_service(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    result = runner.invoke(app, ["vps", "show", "17987", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["id"] == 17987
    assert parsed["backend"] == "cloud"
    # Numeric in JSON, not the formatted string
    assert parsed["amount"] == 17.0
    assert parsed["dedicated_ip"] == "198.51.100.10"


# ── vps status ──────────────────────────────────────────────────────


@respx.mock
def test_status_proxmox_renders_full_metrics(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.get(f"{BASE}/vps/proxmox/17988/status").mock(
        return_value=httpx.Response(200, json=_proxmox_status_envelope()),
    )
    result = runner.invoke(app, ["vps", "status", "17988"])
    assert result.exit_code == 0, result.stderr
    assert "running" in result.stdout
    # CPU usage rendered as percent
    assert "5.0%" in result.stdout
    # Memory rendered with units
    assert "GB" in result.stdout or "MB" in result.stdout
    # Uptime: 3 days
    assert "3d" in result.stdout


@respx.mock
def test_status_cloud_renders_only_power_state(seeded_config: Path) -> None:
    """Cloud backend hits /vps/cloud/{id} (the info endpoint) — after
    1.7 server fix, the response carries `power_state` at the top
    level and CPU/memory/uptime stay None."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    respx.get(f"{BASE}/vps/cloud/17987").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "power_state": "online",
                    # No cpu_usage / memory_used / memory_total / uptime —
                    # the Cloud backend doesn't report runtime metrics.
                },
                "meta": {"request_id": "req_t"},
            },
        )
    )
    result = runner.invoke(app, ["vps", "status", "17987"])
    assert result.exit_code == 0, result.stderr
    assert "online" in result.stdout
    # Missing metrics render as `-`
    assert "-" in result.stdout


@respx.mock
def test_status_json_emits_raw_values(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.get(f"{BASE}/vps/proxmox/17988/status").mock(
        return_value=httpx.Response(
            200,
            json=_proxmox_status_envelope(memory_used=1024 * 1024 * 512),
        )
    )
    result = runner.invoke(
        app, ["vps", "status", "17988", "--output", "json"]
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["power_state"] == "running"
    # Raw bytes — not the human-formatted "512 MB"
    assert parsed["memory_used"] == 1024 * 1024 * 512
    # Raw fraction — not "5.0%"
    assert parsed["cpu_usage"] == 0.05


@respx.mock
def test_status_404_renders_friendly_error(seeded_config: Path) -> None:
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
    result = runner.invoke(app, ["vps", "status", "9999"])
    assert result.exit_code == 1
    assert "VPS service 9999 not found" in result.stderr


# ── shared error path ───────────────────────────────────────────────


def test_vps_with_no_contexts_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["vps", "list"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr


# ── vps power (Phase 3.2) ───────────────────────────────────────────


def _power_ok() -> dict[str, object]:
    return {
        "success": True,
        "data": {},
        "meta": {"request_id": "req_t"},
    }


@respx.mock
def test_start_proxmox_dispatches_to_start_path(seeded_config: Path) -> None:
    """``c.vps.start(id)`` first GETs /account/services/{id} to learn
    the backend, then POSTs /vps/proxmox/{id}/start. Cloud renames
    to /boot — covered separately below."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/start").mock(
        return_value=httpx.Response(200, json=_power_ok())
    )
    result = runner.invoke(app, ["vps", "start", "17988"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Boot request sent for VPS 17988" in result.stdout


@respx.mock
def test_start_cloud_dispatches_to_boot_path(seeded_config: Path) -> None:
    """Cloud backend renames /start → /boot at the HTTP layer; the
    CLI surface stays ``vps start`` either way (the SDK dispatcher
    hides it). Verifies the rewrite happens by asserting the
    /boot route was the one hit."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/boot").mock(
        return_value=httpx.Response(200, json=_power_ok())
    )
    result = runner.invoke(app, ["vps", "start", "17987"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Boot request sent for VPS 17987" in result.stdout


@respx.mock
def test_reboot_hits_reboot_path(seeded_config: Path) -> None:
    """Reboot has the same URL name on both backends — straight
    pass-through with no rewrite."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/reboot").mock(
        return_value=httpx.Response(200, json=_power_ok())
    )
    result = runner.invoke(app, ["vps", "reboot", "17988"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Reboot request sent for VPS 17988" in result.stdout


@respx.mock
def test_shutdown_no_confirm_prompt(seeded_config: Path) -> None:
    """Graceful shutdown is safe by design — the CLI must not
    prompt. Asserts by invoking without any input AND without
    ``--yes``: if a prompt sneaked in, the runner would EOF and
    fail the test."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/shutdown").mock(
        return_value=httpx.Response(200, json=_power_ok())
    )
    result = runner.invoke(app, ["vps", "shutdown", "17988"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Graceful shutdown" in result.stdout


@respx.mock
def test_stop_with_yes_skips_prompt_and_hits_stop_path(
    seeded_config: Path,
) -> None:
    """Proxmox uses ``/stop`` for force-poweroff. ``--yes`` skips
    the corruption-risk confirmation."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/stop").mock(
        return_value=httpx.Response(200, json=_power_ok())
    )
    result = runner.invoke(app, ["vps", "stop", "17988", "--yes"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Force-stop request sent for VPS 17988" in result.stdout


@respx.mock
def test_stop_cloud_dispatches_to_poweroff_path(
    seeded_config: Path,
) -> None:
    """Cloud renames ``/stop`` to ``/poweroff``. Verifies the
    dispatcher rewrote the segment."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/poweroff").mock(
        return_value=httpx.Response(200, json=_power_ok())
    )
    result = runner.invoke(app, ["vps", "stop", "17987", "--yes"])
    assert result.exit_code == 0, result.stderr
    assert route.called


def test_stop_decline_at_prompt_cancels(seeded_config: Path) -> None:
    """Typing ``n`` at the prompt exits 0 with a Cancelled message
    and never hits the API. Mirrors the lock/unlock pattern from
    Phase 3.1."""
    with respx.mock:
        result = runner.invoke(
            app,
            ["vps", "stop", "17988"],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


@respx.mock
def test_start_404_renders_friendly_error(seeded_config: Path) -> None:
    """A service that doesn't exist on the account surfaces as a
    404 from the backend lookup. The CLI maps that to a clean
    one-line stderr (no traceback) and exit 1."""
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
    result = runner.invoke(app, ["vps", "start", "9999"])
    assert result.exit_code == 1
    assert "VPS service 9999 not found" in result.stderr
    assert "Traceback" not in result.stderr


@respx.mock
def test_start_on_non_vps_service_exits_nonzero(seeded_config: Path) -> None:
    """A real service that isn't a VPS (vps_backend=None — e.g. a
    hosting plan) surfaces from the SDK as InvalidRequest. The CLI
    passes the friendly message through."""
    respx.get(f"{BASE}/account/services/15957").mock(
        return_value=httpx.Response(200, json=_service_envelope(_hosting_service()))
    )
    result = runner.invoke(app, ["vps", "start", "15957"])
    assert result.exit_code == 1
    assert "Traceback" not in result.stderr
    stderr_lower = result.stderr.lower()
    assert "vps" in stderr_lower or "not_a_vps" in stderr_lower


# ── vps management (Phase 3.3) ──────────────────────────────────────


def _operation_envelope(
    uuid: str = "op-abc-123",
    status: str = "queued",
    progress: float | None = None,
) -> dict[str, object]:
    """Server response for an endpoint that returns an Operation
    future (Proxmox reinstall, migrate). Mirrors the shape the SDK's
    polling layer validates against (uuid + status, with
    progress/started_at/finished_at/error optional)."""
    return {
        "success": True,
        "data": {
            "uuid": uuid,
            "status": status,
            "progress": progress,
        },
        "meta": {"request_id": "req_t"},
    }


# ── set-hostname ────────────────────────────────────────────────────


@respx.mock
def test_set_hostname_hits_hostname_put_route(seeded_config: Path) -> None:
    """set-hostname is plain — PUT /vps/{backend}/{id}/hostname with
    JSON body. No prompt, both backends share the URL."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.put(f"{BASE}/vps/proxmox/17988/hostname").mock(
        return_value=httpx.Response(
            200, json={"success": True, "data": {}, "meta": {"request_id": "r"}}
        )
    )
    result = runner.invoke(
        app, ["vps", "set-hostname", "17988", "new-host.example.com"]
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "set to 'new-host.example.com'" in result.stdout
    # Body must carry the new hostname
    assert route.calls.last.request.content == b'{"hostname":"new-host.example.com"}'


# ── set-password ────────────────────────────────────────────────────


@respx.mock
def test_set_password_with_flag_skips_prompt(seeded_config: Path) -> None:
    """When ``--password`` is passed on the CLI, no interactive
    prompt should fire. The body lands on /password as JSON."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.put(f"{BASE}/vps/proxmox/17988/password").mock(
        return_value=httpx.Response(
            200, json={"success": True, "data": {}, "meta": {"request_id": "r"}}
        )
    )
    result = runner.invoke(
        app,
        ["vps", "set-password", "17988", "--password", "Sup3rSecret!"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Password updated for VPS 17988" in result.stdout
    assert route.calls.last.request.content == b'{"password":"Sup3rSecret!"}'


@respx.mock
def test_set_password_prompts_when_flag_omitted(seeded_config: Path) -> None:
    """No ``--password`` flag → hidden prompt with confirmation.
    Provide the password twice via stdin (prompt + confirm)."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.put(f"{BASE}/vps/proxmox/17988/password").mock(
        return_value=httpx.Response(
            200, json={"success": True, "data": {}, "meta": {"request_id": "r"}}
        )
    )
    result = runner.invoke(
        app,
        ["vps", "set-password", "17988"],
        input="PromptedPass1!\nPromptedPass1!\n",
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert route.calls.last.request.content == b'{"password":"PromptedPass1!"}'


# ── reinstall ───────────────────────────────────────────────────────


@respx.mock
def test_reinstall_proxmox_without_wait_prints_uuid(seeded_config: Path) -> None:
    """Proxmox reinstall returns an Operation. Without --wait the CLI
    surfaces the uuid and returns immediately so the user can poll
    out-of-band."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/reinstall").mock(
        return_value=httpx.Response(202, json=_operation_envelope(uuid="op-proxmox-1"))
    )
    result = runner.invoke(
        app,
        [
            "vps", "reinstall", "17988",
            "--template", "debian-12",
            "--password", "NewRoot!23",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Reinstall queued for VPS 17988" in result.stdout
    assert "op-proxmox-1" in result.stdout
    # Body must carry template/password/confirm
    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {"template": "debian-12", "password": "NewRoot!23", "confirm": True}


@respx.mock
def test_reinstall_cloud_synchronous(seeded_config: Path) -> None:
    """Cloud reinstall doesn't return an Operation; the CLI prints
    the completion line. --wait is a silent no-op."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    route = respx.post(f"{BASE}/vps/cloud/17987/reinstall").mock(
        return_value=httpx.Response(
            200, json={"success": True, "data": {}, "meta": {"request_id": "r"}}
        )
    )
    result = runner.invoke(
        app,
        [
            "vps", "reinstall", "17987",
            "--template", "ubuntu-22.04",
            "--password", "NewRoot!23",
            "--yes",
            "--wait",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "completed synchronously" in result.stdout


@respx.mock
def test_reinstall_with_wait_polls_operation_to_completion(
    seeded_config: Path,
) -> None:
    """--wait reimplements op.wait() with progress dots. Mock the
    operations endpoint to return 'running' once then 'completed'."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.post(f"{BASE}/vps/proxmox/17988/reinstall").mock(
        return_value=httpx.Response(202, json=_operation_envelope(uuid="op-fast", status="running"))
    )
    # First refresh: still running. Second refresh: completed.
    poll_route = respx.get(f"{BASE}/vps/proxmox/17988/operations/op-fast").mock(
        side_effect=[
            httpx.Response(
                200,
                json=_operation_envelope(uuid="op-fast", status="running", progress=0.5),
            ),
            httpx.Response(
                200,
                json=_operation_envelope(uuid="op-fast", status="completed", progress=1.0),
            ),
        ]
    )

    # Patch time.sleep so the test doesn't actually wait 2s per poll.
    # The poll loop lives in commands._helpers since 3.4.
    import impreza_cli.commands._helpers as vps_mod
    original_sleep = vps_mod.time.sleep
    vps_mod.time.sleep = lambda _: None  # type: ignore[assignment, return-value]
    try:
        result = runner.invoke(
            app,
            [
                "vps", "reinstall", "17988",
                "--template", "debian-12",
                "--password", "NewRoot!23",
                "--yes",
                "--wait",
            ],
        )
    finally:
        vps_mod.time.sleep = original_sleep  # type: ignore[assignment]

    assert result.exit_code == 0, result.stderr
    assert poll_route.called
    assert "Reinstalling VPS 17988" in result.stdout
    assert "done." in result.stdout


@respx.mock
def test_reinstall_wait_surfaces_operation_failure(seeded_config: Path) -> None:
    """When the operation reaches a terminal failure state, --wait
    must exit 1 with a friendly stderr line including the error
    message from the upstream payload."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.post(f"{BASE}/vps/proxmox/17988/reinstall").mock(
        return_value=httpx.Response(
            202, json=_operation_envelope(uuid="op-failboat", status="running")
        )
    )
    respx.get(f"{BASE}/vps/proxmox/17988/operations/op-failboat").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "uuid": "op-failboat",
                    "status": "failed",
                    "progress": 0.42,
                    "error": "template not found",
                },
                "meta": {"request_id": "r"},
            },
        )
    )

    import impreza_cli.commands._helpers as vps_mod
    original_sleep = vps_mod.time.sleep
    vps_mod.time.sleep = lambda _: None  # type: ignore[assignment, return-value]
    try:
        result = runner.invoke(
            app,
            [
                "vps", "reinstall", "17988",
                "--template", "debian-12",
                "--password", "NewRoot!23",
                "--yes",
                "--wait",
            ],
        )
    finally:
        vps_mod.time.sleep = original_sleep  # type: ignore[assignment]

    assert result.exit_code == 1
    assert "Traceback" not in result.stderr
    assert "op-failboat" in result.stderr
    assert "failed" in result.stderr
    assert "template not found" in result.stderr


def test_reinstall_decline_at_prompt_cancels(seeded_config: Path) -> None:
    """Typing ``n`` at the data-loss prompt exits 0 with Cancelled."""
    with respx.mock:
        result = runner.invoke(
            app,
            [
                "vps", "reinstall", "17988",
                "--template", "debian-12",
                "--password", "NewRoot!23",
            ],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


# ── migrate ─────────────────────────────────────────────────────────


@respx.mock
def test_migrate_proxmox_without_wait_prints_uuid(seeded_config: Path) -> None:
    """Migrate returns an Operation on Proxmox; without --wait we
    print the uuid and return."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/migrate").mock(
        return_value=httpx.Response(202, json=_operation_envelope(uuid="op-mig-1"))
    )
    result = runner.invoke(
        app,
        ["vps", "migrate", "17988", "--target", "node-7", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Migration queued" in result.stdout
    assert "op-mig-1" in result.stdout
    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {"target": "node-7"}


@respx.mock
def test_migrate_cloud_exits_with_backend_not_supported(
    seeded_config: Path,
) -> None:
    """Cloud VPS → SDK raises BackendNotSupported → CLI exits 1
    with a friendly Proxmox-only message."""
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    result = runner.invoke(
        app,
        ["vps", "migrate", "17987", "--target", "node-7", "--yes"],
    )
    assert result.exit_code == 1
    assert "Cloud backend" in result.stderr
    assert "Proxmox-only" in result.stderr


# No suspend / unsuspend tests: the server-side endpoints were retired
# on 2026-05-11 (customer-facing suspend / unsuspend on Proxmox VPS was
# removed from the API surface). The SDK and CLI followed.


# ── cancel ──────────────────────────────────────────────────────────


@respx.mock
def test_cancel_defaults_to_end_of_billing_period(seeded_config: Path) -> None:
    """Default --type is 'End of Billing Period' — protect prepaid
    days from accidental immediate cancellation."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/cancel").mock(
        return_value=httpx.Response(
            200, json={"success": True, "data": {}, "meta": {"request_id": "r"}}
        )
    )
    result = runner.invoke(app, ["vps", "cancel", "17988", "--yes"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {"type": "End of Billing Period"}


@respx.mock
def test_cancel_immediate_with_reason(seeded_config: Path) -> None:
    """--type Immediate + --reason sends both in the body."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/cancel").mock(
        return_value=httpx.Response(
            200, json={"success": True, "data": {}, "meta": {"request_id": "r"}}
        )
    )
    result = runner.invoke(
        app,
        [
            "vps", "cancel", "17988",
            "--type", "Immediate",
            "--reason", "moving providers",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    import json as _json
    body = _json.loads(route.calls.last.request.content)
    assert body == {"type": "Immediate", "reason": "moving providers"}


def test_cancel_invalid_type_exits_nonzero(seeded_config: Path) -> None:
    """The CLI validates --type client-side before any HTTP call.
    Unknown value → exit 1 with a friendly stderr line."""
    result = runner.invoke(
        app, ["vps", "cancel", "17988", "--type", "Whenever", "--yes"]
    )
    assert result.exit_code == 1
    assert "--type must be one of" in result.stderr


def test_cancel_decline_at_prompt(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app, ["vps", "cancel", "17988"], input="n\n"
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout
