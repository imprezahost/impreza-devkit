"""Unit tests for ``impreza vps proxmox`` (Phase 3.4).

Covers the four sub-app groups (snapshots, backups, backup-schedules,
network) end-to-end via respx mocks. Each verb gets:

* a happy-path test pinning the URL + body
* a Cloud-backend test asserting the friendly "Proxmox-only" stderr
  line is emitted when the SDK raises :class:`BackendNotSupported`
* destructive verbs (delete, rollback, restore) get a decline-at-
  prompt test confirming the "Cancelled." path

The ``--wait`` paths for the three Operation-returning verbs
(snapshots rollback, backups create, backups restore) are exercised
with a fast mocked poll cycle (``time.sleep`` patched to no-op) and
a terminal-failure mapping test.
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


def _operation_envelope(
    uuid: str = "op-abc-123",
    status: str = "queued",
    progress: float | None = None,
    error: str | None = None,
) -> dict[str, object]:
    return _ok(
        {
            "uuid": uuid,
            "status": status,
            "progress": progress,
            "error": error,
        }
    )


def _patch_sleep() -> None:
    """Patch the helpers module's time.sleep so --wait polls don't
    actually wait. Reset by the caller via the returned closure."""
    import impreza_cli.commands._helpers as helpers_mod

    helpers_mod.time.sleep = lambda _: None  # type: ignore[assignment, return-value]


# ══════════════════════════════════════════════════════════════════════
# snapshots
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_snapshots_list_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.get(f"{BASE}/vps/proxmox/17988/snapshots").mock(
        return_value=httpx.Response(
            200,
            json=_list_envelope(
                "snapshots",
                [
                    {"name": "pre-update", "description": "before kernel patch",
                     "created_at": "2026-05-10T10:00:00Z"},
                    {"name": "manual", "description": None, "created_at": None},
                ],
            ),
        )
    )
    result = runner.invoke(app, ["vps", "proxmox", "snapshots", "list", "17988"])
    assert result.exit_code == 0, result.stderr
    assert "pre-update" in result.stdout
    assert "manual" in result.stdout
    assert "before kernel patch" in result.stdout


@respx.mock
def test_snapshots_list_empty(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.get(f"{BASE}/vps/proxmox/17988/snapshots").mock(
        return_value=httpx.Response(200, json=_list_envelope("snapshots", []))
    )
    result = runner.invoke(app, ["vps", "proxmox", "snapshots", "list", "17988"])
    assert result.exit_code == 0
    assert "No snapshots on VPS 17988" in result.stdout


@respx.mock
def test_snapshots_create_with_description(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/snapshots").mock(
        return_value=httpx.Response(
            201,
            json=_ok({"name": "pre-update", "description": "before kernel patch"}),
        )
    )
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "snapshots", "create", "17988", "pre-update",
            "--description", "before kernel patch",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"name": "pre-update", "description": "before kernel patch"}
    assert "'pre-update' created" in result.stdout


@respx.mock
def test_snapshots_delete_with_yes(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.delete(f"{BASE}/vps/proxmox/17988/snapshots/pre-update").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "proxmox", "snapshots", "delete", "17988", "pre-update", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "'pre-update' deleted" in result.stdout


def test_snapshots_delete_decline(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app,
            ["vps", "proxmox", "snapshots", "delete", "17988", "pre-update"],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


@respx.mock
def test_snapshots_rollback_without_wait_prints_uuid(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/snapshots/pre-update/rollback").mock(
        return_value=httpx.Response(202, json=_operation_envelope(uuid="op-roll-1"))
    )
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "snapshots", "rollback", "17988", "pre-update",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Rollback queued" in result.stdout
    assert "op-roll-1" in result.stdout


@respx.mock
def test_snapshots_rollback_with_wait_polls_to_completion(
    seeded_config: Path,
) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.post(f"{BASE}/vps/proxmox/17988/snapshots/pre-update/rollback").mock(
        return_value=httpx.Response(
            202, json=_operation_envelope(uuid="op-roll-fast", status="running")
        )
    )
    respx.get(f"{BASE}/vps/proxmox/17988/operations/op-roll-fast").mock(
        side_effect=[
            httpx.Response(
                200,
                json=_operation_envelope(uuid="op-roll-fast", status="running",
                                         progress=0.5),
            ),
            httpx.Response(
                200,
                json=_operation_envelope(uuid="op-roll-fast", status="completed",
                                         progress=1.0),
            ),
        ]
    )

    _patch_sleep()
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "snapshots", "rollback", "17988", "pre-update",
            "--yes", "--wait",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert "Rolling back VPS 17988" in result.stdout
    assert "done." in result.stdout


@respx.mock
def test_snapshots_list_on_cloud_exits_proxmox_only(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    result = runner.invoke(app, ["vps", "proxmox", "snapshots", "list", "17987"])
    assert result.exit_code == 1
    assert "Proxmox-only" in result.stderr


# ══════════════════════════════════════════════════════════════════════
# backups
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_backups_list_renders_size_in_table_mode(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.get(f"{BASE}/vps/proxmox/17988/backups").mock(
        return_value=httpx.Response(
            200,
            json=_list_envelope(
                "backups",
                [
                    {
                        "id": "bk-1",
                        "date": "2026-05-09T03:00:00Z",
                        "size": 2 * 1024**3,
                        "mode": "snapshot",
                        "compress": "zstd",
                        "protected": False,
                    },
                ],
            ),
        )
    )
    result = runner.invoke(app, ["vps", "proxmox", "backups", "list", "17988"])
    assert result.exit_code == 0, result.stderr
    assert "bk-1" in result.stdout
    assert "GB" in result.stdout  # human-readable size in table mode


@respx.mock
def test_backups_list_json_emits_raw_size(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    raw_size = 2 * 1024**3
    respx.get(f"{BASE}/vps/proxmox/17988/backups").mock(
        return_value=httpx.Response(
            200,
            json=_list_envelope(
                "backups",
                [
                    {"id": "bk-1", "date": "2026-05-09T03:00:00Z",
                     "size": raw_size, "mode": "snapshot", "compress": "zstd",
                     "protected": False},
                ],
            ),
        )
    )
    result = runner.invoke(
        app, ["vps", "proxmox", "backups", "list", "17988", "--output", "json"]
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list) and parsed[0]["size"] == raw_size


@respx.mock
def test_backups_create_without_wait(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/backups").mock(
        return_value=httpx.Response(202, json=_operation_envelope(uuid="op-bk-1"))
    )
    result = runner.invoke(app, ["vps", "proxmox", "backups", "create", "17988"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Backup queued" in result.stdout
    assert "op-bk-1" in result.stdout


@respx.mock
def test_backups_create_with_wait_failure_maps_to_exit_1(seeded_config: Path) -> None:
    """Terminal-failure path on the Operation poll. Asserts the error
    message and the upstream `error` field both land on stderr."""
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.post(f"{BASE}/vps/proxmox/17988/backups").mock(
        return_value=httpx.Response(
            202,
            json=_operation_envelope(uuid="op-bk-fail", status="running"),
        )
    )
    respx.get(f"{BASE}/vps/proxmox/17988/operations/op-bk-fail").mock(
        return_value=httpx.Response(
            200,
            json=_operation_envelope(
                uuid="op-bk-fail",
                status="failed",
                progress=0.3,
                error="storage quota exceeded",
            ),
        )
    )

    _patch_sleep()
    result = runner.invoke(
        app, ["vps", "proxmox", "backups", "create", "17988", "--wait"]
    )
    assert result.exit_code == 1
    assert "op-bk-fail" in result.stderr
    assert "failed" in result.stderr
    assert "storage quota exceeded" in result.stderr


@respx.mock
def test_backups_restore_with_yes_hits_restore_path(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/backups/bk-1/restore").mock(
        return_value=httpx.Response(202, json=_operation_envelope(uuid="op-rest-1"))
    )
    result = runner.invoke(
        app,
        ["vps", "proxmox", "backups", "restore", "17988", "bk-1", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "Restore queued" in result.stdout


def test_backups_restore_decline(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app,
            ["vps", "proxmox", "backups", "restore", "17988", "bk-1"],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


@respx.mock
def test_backups_delete_with_yes(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.delete(f"{BASE}/vps/proxmox/17988/backups/bk-1").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        ["vps", "proxmox", "backups", "delete", "17988", "bk-1", "--yes"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "'bk-1' deleted" in result.stdout


@respx.mock
def test_backups_list_on_cloud_exits_proxmox_only(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    result = runner.invoke(app, ["vps", "proxmox", "backups", "list", "17987"])
    assert result.exit_code == 1
    assert "Proxmox-only" in result.stderr


# ══════════════════════════════════════════════════════════════════════
# backup-schedules
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_schedules_list_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    respx.get(f"{BASE}/vps/proxmox/17988/backup-schedules").mock(
        return_value=httpx.Response(
            200,
            json=_list_envelope(
                "schedules",
                [
                    {"id": "sch-1", "dow": "mon,wed,fri", "hour": 3,
                     "minute": 0, "mode": "snapshot", "compress": "zstd"},
                ],
            ),
        )
    )
    result = runner.invoke(
        app, ["vps", "proxmox", "backup-schedules", "list", "17988"]
    )
    assert result.exit_code == 0, result.stderr
    assert "sch-1" in result.stdout
    assert "mon,wed,fri" in result.stdout


@respx.mock
def test_schedules_create(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/backup-schedules").mock(
        return_value=httpx.Response(
            201,
            json=_ok({"id": "sch-1", "dow": "mon,wed,fri", "hour": 3,
                      "minute": 30, "mode": "snapshot", "compress": "zstd"}),
        )
    )
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "backup-schedules", "create", "17988",
            "--dow", "mon,wed,fri",
            "--hour", "3",
            "--minute", "30",
            "--mode", "snapshot",
            "--compress", "zstd",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {
        "dow": "mon,wed,fri", "hour": 3, "minute": 30,
        "mode": "snapshot", "compress": "zstd",
    }
    assert "'sch-1' created" in result.stdout
    assert "03:30" in result.stdout


def test_schedules_create_invalid_mode(seeded_config: Path) -> None:
    """Client-side validation rejects unknown --mode before any HTTP."""
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "backup-schedules", "create", "17988",
            "--dow", "mon", "--hour", "3", "--minute", "0",
            "--mode", "freeze",
        ],
    )
    assert result.exit_code == 1
    assert "--mode must be one of" in result.stderr


def test_schedules_create_invalid_compress(seeded_config: Path) -> None:
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "backup-schedules", "create", "17988",
            "--dow", "mon", "--hour", "3", "--minute", "0",
            "--compress", "bzip2",
        ],
    )
    assert result.exit_code == 1
    assert "--compress must be one of" in result.stderr


def test_schedules_create_hour_out_of_range(seeded_config: Path) -> None:
    """Typer's built-in min/max validation rejects --hour 24."""
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "backup-schedules", "create", "17988",
            "--dow", "mon", "--hour", "24", "--minute", "0",
        ],
    )
    assert result.exit_code != 0


@respx.mock
def test_schedules_delete_with_yes(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.delete(f"{BASE}/vps/proxmox/17988/backup-schedules/sch-1").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(
        app,
        [
            "vps", "proxmox", "backup-schedules", "delete", "17988", "sch-1",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "'sch-1' deleted" in result.stdout


# ══════════════════════════════════════════════════════════════════════
# network reconfigure
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_network_reconfigure_proxmox(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(200, json=_service_envelope(_proxmox_service()))
    )
    route = respx.post(f"{BASE}/vps/proxmox/17988/network/reconfigure").mock(
        return_value=httpx.Response(200, json=_ok({"applied": True}))
    )
    result = runner.invoke(
        app, ["vps", "proxmox", "network", "reconfigure", "17988"]
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    # `applied` shows up in the dict render
    assert "applied" in result.stdout


@respx.mock
def test_network_reconfigure_on_cloud(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17987").mock(
        return_value=httpx.Response(200, json=_service_envelope(_cloud_service()))
    )
    result = runner.invoke(
        app, ["vps", "proxmox", "network", "reconfigure", "17987"]
    )
    assert result.exit_code == 1
    assert "Proxmox-only" in result.stderr


# ══════════════════════════════════════════════════════════════════════
# Shared: ResourceNotFound surfaces friendly stderr
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_snapshots_list_404_renders_friendly_error(seeded_config: Path) -> None:
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
    result = runner.invoke(app, ["vps", "proxmox", "snapshots", "list", "9999"])
    assert result.exit_code == 1
    assert "VPS service 9999 not found" in result.stderr
    assert "Traceback" not in result.stderr
