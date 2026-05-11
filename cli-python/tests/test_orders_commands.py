"""Unit tests for ``impreza order`` (Phase 3.6).

Covers the four shipping verbs (list, show, create, upgrade) via
respx mocks. Smart name/id resolution from 1.4d is tested by
asserting that create with name-keyed --config-option triggers an
extra GET /products/{id} for resolution; pure-int dicts skip it.
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


def _ok(payload: dict[str, object] | None = None) -> dict[str, object]:
    return {
        "success": True,
        "data": payload if payload is not None else {},
        "meta": {"request_id": "req_t"},
    }


# ── order list ──────────────────────────────────────────────────────


@respx.mock
def test_list_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/orders").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "orders": [
                    {"id": 1, "order_number": "ORD-0001", "date": "2026-05-09",
                     "amount": 25.0, "invoice_id": 200, "status": "Active",
                     "payment_method": "mailin"},
                    {"id": 2, "order_number": 1000000002, "date": "2026-05-10",
                     "amount": 5.0, "invoice_id": 201, "status": "Pending",
                     "payment_method": None},
                ],
            }),
        )
    )
    result = runner.invoke(app, ["order", "list"])
    assert result.exit_code == 0, result.stderr
    assert "ORD-0001" in result.stdout
    assert "1000000002" in result.stdout
    assert "Active" in result.stdout
    assert "Pending" in result.stdout


@respx.mock
def test_list_with_status_filter_passes_query_param(seeded_config: Path) -> None:
    route = respx.get(f"{BASE}/orders", params={"status": "Active"}).mock(
        return_value=httpx.Response(200, json=_ok({"orders": []}))
    )
    result = runner.invoke(app, ["order", "list", "--status", "Active"])
    assert result.exit_code == 0
    assert route.called
    assert "No orders match status 'Active'" in result.stdout


@respx.mock
def test_list_empty_default_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/orders").mock(
        return_value=httpx.Response(200, json=_ok({"orders": []}))
    )
    result = runner.invoke(app, ["order", "list"])
    assert result.exit_code == 0
    assert "No orders on this account" in result.stdout


@respx.mock
def test_list_json_output(seeded_config: Path) -> None:
    respx.get(f"{BASE}/orders").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "orders": [
                    {"id": 7, "order_number": "ORD-7", "date": "2026-05-01",
                     "amount": 100.0, "status": "Active"},
                ],
            }),
        )
    )
    result = runner.invoke(app, ["order", "list", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed[0]["id"] == 7
    assert parsed[0]["amount"] == 100.0


# ── order show ──────────────────────────────────────────────────────


@respx.mock
def test_show_renders_summary_and_items(seeded_config: Path) -> None:
    respx.get(f"{BASE}/orders/42").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "id": 42,
                "order_number": "ORD-0042",
                "date": "2026-05-09",
                "amount": 42.0,
                "invoice_id": 200,
                "status": "Active",
                "payment_method": "mailin",
                "items": [
                    {"service_id": 17988, "domain": "vps-1.example.com",
                     "product": "Hong Kong VPS I", "status": "Active",
                     "billing_cycle": "Monthly", "amount": 25.0},
                    {"service_id": 17987, "domain": "vps-2.example.com",
                     "product": "VPS I", "status": "Active",
                     "billing_cycle": "Monthly", "amount": 17.0},
                ],
            }),
        )
    )
    result = runner.invoke(app, ["order", "show", "42"])
    assert result.exit_code == 0, result.stderr
    # Summary + line items both rendered. The table formatter may
    # truncate long product names, so just check the rendered service
    # ids + domains + cycle that aren't aggressively shortened.
    assert "ORD-0042" in result.stdout
    assert "17988" in result.stdout
    assert "17987" in result.stdout
    assert "Monthly" in result.stdout


@respx.mock
def test_show_json_emits_full_detail(seeded_config: Path) -> None:
    respx.get(f"{BASE}/orders/42").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "id": 42, "amount": 42.0, "status": "Active",
                "items": [{"service_id": 1, "product": "p"}],
            }),
        )
    )
    result = runner.invoke(app, ["order", "show", "42", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["id"] == 42
    assert isinstance(parsed["items"], list) and len(parsed["items"]) == 1


# ── order create ────────────────────────────────────────────────────


@respx.mock
def test_create_pure_id_dict_skips_product_lookup(seeded_config: Path) -> None:
    """Passing --config-option / --custom-field with pure-int sides
    should NOT trigger the GET /products/{id} resolution call."""
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(
            201,
            json=_ok({
                "order_id": 100, "invoice_id": 200,
                "amount": 25.0, "currency": "USD",
                "status": "Active",
                "product": "Hong Kong VPS I",
            }),
        )
    )
    # If the SDK tried to fetch the product, this route would 404.
    product_route = respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(404)
    )
    result = runner.invoke(
        app,
        [
            "order", "create",
            "--product-id", "12",
            "--billing-cycle", "monthly",
            "--config-option", "3=5",
            "--custom-field", "1=myserver.example.com",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert create_route.called
    assert not product_route.called
    body = json.loads(create_route.calls.last.request.content)
    assert body["product_id"] == 12
    assert body["billing_cycle"] == "monthly"
    assert body["config_options"] == {"3": 5}
    assert body["custom_fields"] == {"1": "myserver.example.com"}
    assert "Order 100 created" in result.stdout


@respx.mock
def test_create_name_keyed_triggers_product_resolution(seeded_config: Path) -> None:
    """Name-keyed dicts force a GET /products/{id} to resolve names
    against the product's config-options / custom-fields catalog."""
    respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "id": 12, "name": "Test VPS", "type": "server",
                "currency": "USD",
                "config_options": [
                    {"id": 3, "name": "Disk Space", "type": 0,
                     "options": [{"id": 5, "name": "20 GB"},
                                 {"id": 6, "name": "40 GB"}]},
                ],
                "custom_fields": [
                    {"id": 1, "name": "Hostname",
                     "type": "text", "required": True},
                ],
            }),
        )
    )
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(
            201,
            json=_ok({
                "order_id": 101, "invoice_id": 201,
                "amount": 25.0, "currency": "USD", "status": "Active",
            }),
        )
    )
    result = runner.invoke(
        app,
        [
            "order", "create",
            "--product-id", "12",
            "--billing-cycle", "monthly",
            "--config-option", "Disk Space=20 GB",
            "--custom-field", "Hostname=myserver.example.com",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert create_route.called
    body = json.loads(create_route.calls.last.request.content)
    # Names resolved to IDs
    assert body["config_options"] == {"3": 5}
    assert body["custom_fields"] == {"1": "myserver.example.com"}


@respx.mock
def test_create_invalid_billing_cycle(seeded_config: Path) -> None:
    """Client-side validation rejects unknown billing cycles before
    any HTTP call."""
    result = runner.invoke(
        app,
        [
            "order", "create",
            "--product-id", "12",
            "--billing-cycle", "weekly",
            "--yes",
        ],
    )
    assert result.exit_code == 1
    assert "--billing-cycle must be one of" in result.stderr


def test_create_decline_at_prompt(seeded_config: Path) -> None:
    """Typing n at the balance-debit prompt exits 0 without HTTP."""
    with respx.mock:
        result = runner.invoke(
            app,
            [
                "order", "create",
                "--product-id", "12",
                "--billing-cycle", "monthly",
            ],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


@respx.mock
def test_create_insufficient_credit_hint(seeded_config: Path) -> None:
    """402 InsufficientCredit gets the topup-hint stderr line."""
    respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(
            402,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {
                    "code": "INSUFFICIENT_CREDIT",
                    "message": "Your balance is too low to cover this order.",
                },
            },
        )
    )
    result = runner.invoke(
        app,
        [
            "order", "create",
            "--product-id", "12",
            "--billing-cycle", "monthly",
            "--yes",
        ],
    )
    assert result.exit_code == 1
    assert "balance is too low" in result.stderr
    assert "impreza account topup" in result.stderr


def test_create_malformed_kv_option(seeded_config: Path) -> None:
    """`--config-option foo` (no `=`) rejected before HTTP."""
    result = runner.invoke(
        app,
        [
            "order", "create",
            "--product-id", "12",
            "--billing-cycle", "monthly",
            "--config-option", "no_equals_here",
            "--yes",
        ],
    )
    assert result.exit_code == 1
    assert "KEY=VALUE" in result.stderr


# ── order upgrade ───────────────────────────────────────────────────


@respx.mock
def test_upgrade_with_yes(seeded_config: Path) -> None:
    route = respx.post(f"{BASE}/orders/17988/upgrade").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "order_id": 200, "invoice_id": 300,
                "amount": 10.0, "currency": "USD", "status": "Active",
            }),
        )
    )
    result = runner.invoke(
        app,
        [
            "order", "upgrade",
            "--service-id", "17988",
            "--new-product-id", "20",
            "--billing-cycle", "annually",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {
        "service_id": 17988,
        "new_product_id": 20,
        "billing_cycle": "annually",
    }
    assert "Service 17988 upgrade order 200" in result.stdout


def test_upgrade_decline_at_prompt(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app,
            [
                "order", "upgrade",
                "--service-id", "17988",
                "--new-product-id", "20",
                "--billing-cycle", "annually",
            ],
            input="n\n",
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


def test_upgrade_invalid_billing_cycle(seeded_config: Path) -> None:
    result = runner.invoke(
        app,
        [
            "order", "upgrade",
            "--service-id", "17988",
            "--new-product-id", "20",
            "--billing-cycle", "yearly",
            "--yes",
        ],
    )
    assert result.exit_code == 1
    assert "--billing-cycle must be one of" in result.stderr
