"""Tests for ``--output yaml`` across every command group.

These tests confirm that:

1. YAML output round-trips cleanly through ``yaml.safe_load`` —
   what comes out the other side is the same Python object the
   JSON path would emit.
2. Every resource group's commands actually go through the YAML
   render path (some had bugs earlier where they only branched
   between TABLE and JSON; YAML used to fall through to TABLE).
3. The :func:`impreza_cli.output._yaml_dump` helper stays
   importable even on installs without ``pyyaml`` — the
   ImportError is raised lazily on use, not at import time.

The respx mocks are minimal — just enough to feed each command a
plausible response. The assertion is on shape parity with JSON:
``yaml.safe_load(yaml_output) == json.loads(json_output)``.
"""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest
import respx
import yaml
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
    cfg.add_context("p", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()
    return isolated_config


def _ok(data: object) -> dict[str, object]:
    return {"success": True, "data": data, "meta": {"request_id": "r"}}


# ── account ─────────────────────────────────────────────────────────


@respx.mock
def test_yaml_round_trip_account_info(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "id": 1,
                    "first_name": "Test",
                    "last_name": "User",
                    "email": "test@example.com",
                    "balance": 5.0,
                    "currency": "USD",
                    "registered_at": "2024-01-15",
                }
            ),
        )
    )
    json_result = runner.invoke(app, ["account", "info", "--output", "json"])
    yaml_result = runner.invoke(app, ["account", "info", "--output", "yaml"])
    assert json_result.exit_code == 0
    assert yaml_result.exit_code == 0
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


@respx.mock
def test_yaml_round_trip_account_services(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "services": [
                        {
                            "id": 17988,
                            "domain": "vps.example.com",
                            "status": "Active",
                            "product": "VPS I",
                            "billing_cycle": "Monthly",
                            "amount": 25.0,
                            "registered_at": "2026-05-09",
                            "next_due": "2026-06-09",
                            "vps_backend": "proxmox",
                        }
                    ],
                    "total": 1,
                }
            ),
        )
    )
    json_result = runner.invoke(app, ["account", "services", "--output", "json"])
    yaml_result = runner.invoke(app, ["account", "services", "--output", "yaml"])
    assert yaml_result.exit_code == 0, yaml_result.stderr
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


# ── catalog ─────────────────────────────────────────────────────────


@respx.mock
def test_yaml_round_trip_catalog_product_groups(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products/groups").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {"groups": [{"id": 1, "name": "Hosting", "product_count": 12}]}
            ),
        )
    )
    json_result = runner.invoke(
        app, ["catalog", "product-groups", "--output", "json"]
    )
    yaml_result = runner.invoke(
        app, ["catalog", "product-groups", "--output", "yaml"]
    )
    assert yaml_result.exit_code == 0
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


# ── domain ──────────────────────────────────────────────────────────


@respx.mock
def test_yaml_round_trip_domain_show(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/example.com").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "domain": "example.com",
                    "status": "Active",
                    "registration_date": "2024-01-15",
                    "expires_at": "2027-01-15",
                    "next_due_date": "2027-01-15",
                    "nameservers": ["ns1.example.com", "ns2.example.com"],
                    "lock_status": True,
                    "id_protection": True,
                    "auto_renew": False,
                    "privacy": True,
                    "epp_code": None,
                }
            ),
        )
    )
    json_result = runner.invoke(
        app, ["domain", "show", "example.com", "--output", "json"]
    )
    yaml_result = runner.invoke(
        app, ["domain", "show", "example.com", "--output", "yaml"]
    )
    assert yaml_result.exit_code == 0
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


# ── vps ─────────────────────────────────────────────────────────────


@respx.mock
def test_yaml_round_trip_vps_status(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/services/17988").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "id": 17988,
                    "domain": "vps.example.com",
                    "status": "Active",
                    "product": "VPS I",
                    "billing_cycle": "Monthly",
                    "amount": 25.0,
                    "registered_at": "2026-05-09",
                    "next_due": "2026-06-09",
                    "vps_backend": "proxmox",
                }
            ),
        )
    )
    respx.get(f"{BASE}/vps/proxmox/17988/status").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "power_state": "running",
                    "cpu_usage": 0.05,
                    "memory_used": 1024 * 1024 * 1024,
                    "memory_total": 2 * 1024 * 1024 * 1024,
                    "uptime": 86400 * 3,
                }
            ),
        )
    )
    json_result = runner.invoke(
        app, ["vps", "status", "17988", "--output", "json"]
    )
    yaml_result = runner.invoke(
        app, ["vps", "status", "17988", "--output", "yaml"]
    )
    assert yaml_result.exit_code == 0
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


# ── invoice ─────────────────────────────────────────────────────────


@respx.mock
def test_yaml_round_trip_invoice_show(seeded_config: Path) -> None:
    respx.get(f"{BASE}/invoices/12031").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "id": 12031,
                    "invoice_num": "12031",
                    "date": "2026-04-01",
                    "due_date": "2026-04-15",
                    "date_paid": "2026-04-02",
                    "subtotal": 25.0,
                    "credit": 0.0,
                    "tax": 0.0,
                    "total": 25.0,
                    "status": "Paid",
                    "payment_method": "btcpayinline",
                    "items": [
                        {
                            "id": 1,
                            "type": "Hosting",
                            "description": "Test",
                            "amount": 25.0,
                            "taxed": False,
                        }
                    ],
                    "transactions": [],
                }
            ),
        )
    )
    json_result = runner.invoke(
        app, ["invoice", "show", "12031", "--output", "json"]
    )
    yaml_result = runner.invoke(
        app, ["invoice", "show", "12031", "--output", "yaml"]
    )
    assert yaml_result.exit_code == 0
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


# ── key ─────────────────────────────────────────────────────────────


@respx.mock
def test_yaml_round_trip_key_whoami(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/api-keys/self").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "id": 21,
                    "client_id": 1,
                    "prefix": "imp_a1b2c3d4",
                    "label": "ci-bot",
                    "status": "active",
                    "last_used_at": "2026-05-09",
                    "created_at": "2026-05-08",
                    "rate_limit_per_minute": 60,
                    "ip_whitelist": [
                        {
                            "id": 1,
                            "ip_address": "1.2.3.4",
                            "label": "office",
                            "created_at": "2026-04-01",
                        }
                    ],
                    "request_ip": "1.2.3.4",
                }
            ),
        )
    )
    json_result = runner.invoke(app, ["key", "whoami", "--output", "json"])
    yaml_result = runner.invoke(app, ["key", "whoami", "--output", "yaml"])
    assert yaml_result.exit_code == 0
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


# ── context list (uses print_table list-of-dicts path) ─────────────


def test_yaml_round_trip_context_list(isolated_config: Path) -> None:
    """Hits the print_table list path rather than print_dict — covers
    the second YAML branch in output.py."""
    cfg = Config.load(isolated_config)
    cfg.add_context("p", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()

    json_result = runner.invoke(app, ["context", "list", "--output", "json"])
    yaml_result = runner.invoke(app, ["context", "list", "--output", "yaml"])
    assert yaml_result.exit_code == 0, yaml_result.stderr
    assert yaml.safe_load(yaml_result.stdout) == json.loads(json_result.stdout)


# ── ImportError fallback (pyyaml missing) ──────────────────────────


def test_yaml_dump_raises_clear_runtime_error_when_pyyaml_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """When pyyaml isn't installed, _yaml_dump raises a clear
    RuntimeError pointing at the install hint. Simulated by
    blocking the import."""
    import builtins

    real_import = builtins.__import__

    def blocked_import(name: str, *args: object, **kwargs: object) -> object:
        if name == "yaml":
            raise ImportError("simulated missing pyyaml")
        return real_import(name, *args, **kwargs)  # type: ignore[arg-type]

    monkeypatch.setattr(builtins, "__import__", blocked_import)

    from impreza_cli.output import _yaml_dump

    with pytest.raises(RuntimeError) as exc_info:
        _yaml_dump({"hello": "world"})
    assert "pyyaml" in str(exc_info.value).lower()
    assert "install" in str(exc_info.value).lower()
