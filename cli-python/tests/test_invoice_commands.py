"""Unit tests for ``impreza invoice`` commands.

respx-mocked HTTP, isolated config (via the ``isolated_config``
fixture from conftest.py), assertions on exit code + stdout/stderr.
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


def _list_envelope(invoices: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"invoices": invoices, "total": len(invoices)},
        "meta": {"request_id": "req_t"},
    }


def _detail_envelope(invoice: dict[str, object]) -> dict[str, object]:
    return {
        "success": True,
        "data": invoice,
        "meta": {"request_id": "req_t"},
    }


def _sample_invoice(
    id_: int = 12031,
    status: str = "Paid",
    total: float = 25.00,
) -> dict[str, object]:
    return {
        "id": id_,
        "invoice_num": str(id_),
        "date": "2026-04-01",
        "due_date": "2026-04-15",
        "date_paid": "2026-04-02" if status == "Paid" else None,
        "subtotal": total,
        "credit": 0.0,
        "tax": 0.0,
        "total": total,
        "status": status,
        "payment_method": "btcpayinline",
    }


# ── invoice list ────────────────────────────────────────────────────


@respx.mock
def test_list_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(
            200,
            json=_list_envelope(
                [
                    _sample_invoice(12031, "Paid", 25.00),
                    _sample_invoice(12032, "Unpaid", 17.00),
                ]
            ),
        )
    )
    result = runner.invoke(app, ["invoice", "list"])
    assert result.exit_code == 0, result.stderr
    assert "12031" in result.stdout
    assert "12032" in result.stdout
    assert "Paid" in result.stdout
    assert "Unpaid" in result.stdout
    assert "25.00" in result.stdout


@respx.mock
def test_list_filter_passes_status_to_api(seeded_config: Path) -> None:
    route = respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(200, json=_list_envelope([])),
    )
    result = runner.invoke(app, ["invoice", "list", "--status", "Unpaid"])
    assert result.exit_code == 0
    url = str(route.calls.last.request.url)
    assert "status=Unpaid" in url


@respx.mock
def test_list_empty_default_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(200, json=_list_envelope([])),
    )
    result = runner.invoke(app, ["invoice", "list"])
    assert result.exit_code == 0
    assert "No invoices on this account yet" in result.stdout


@respx.mock
def test_list_empty_with_filter_mentions_filter(seeded_config: Path) -> None:
    respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(200, json=_list_envelope([])),
    )
    result = runner.invoke(app, ["invoice", "list", "--status", "Unpaid"])
    assert result.exit_code == 0
    assert "No invoices with status 'Unpaid'" in result.stdout


@respx.mock
def test_list_json_output(seeded_config: Path) -> None:
    respx.get(f"{BASE}/invoices").mock(
        return_value=httpx.Response(
            200, json=_list_envelope([_sample_invoice()])
        )
    )
    result = runner.invoke(app, ["invoice", "list", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list) and len(parsed) == 1
    assert parsed[0]["id"] == 12031
    # Numeric in JSON, not formatted
    assert parsed[0]["total"] == 25.00


# ── invoice show ────────────────────────────────────────────────────


@respx.mock
def test_show_renders_full_detail(seeded_config: Path) -> None:
    detail = _sample_invoice()
    detail["items"] = [
        {
            "id": 1,
            "type": "Hosting",
            "description": "USA Linux Hosting III - example.com (01/04/2026 to 01/04/2027)",
            "amount": 25.00,
            "taxed": False,
        }
    ]
    detail["transactions"] = [
        {
            "id": 5001,
            "date": "2026-04-02",
            "gateway": "btcpayinline",
            "amount": 25.00,
            "transaction_id": "btc_tx_abc123",
        }
    ]
    respx.get(f"{BASE}/invoices/12031").mock(
        return_value=httpx.Response(200, json=_detail_envelope(detail)),
    )
    result = runner.invoke(app, ["invoice", "show", "12031"])
    assert result.exit_code == 0, result.stderr
    # Header
    assert "12031" in result.stdout
    assert "Paid" in result.stdout
    # Line item
    assert "Hosting" in result.stdout
    # Transaction
    assert "btcpayinline" in result.stdout


@respx.mock
def test_show_handles_invoice_with_no_items(seeded_config: Path) -> None:
    """Empty items / transactions arrays should not crash the
    render — the sub-table sections are simply skipped."""
    detail = _sample_invoice(99, "Cancelled", 0.0)
    detail["items"] = []
    detail["transactions"] = []
    respx.get(f"{BASE}/invoices/99").mock(
        return_value=httpx.Response(200, json=_detail_envelope(detail)),
    )
    result = runner.invoke(app, ["invoice", "show", "99"])
    assert result.exit_code == 0, result.stderr
    assert "Cancelled" in result.stdout


@respx.mock
def test_show_404_renders_friendly_error(seeded_config: Path) -> None:
    respx.get(f"{BASE}/invoices/9999").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {"code": "NOT_FOUND", "message": "Invoice not found."},
            },
        )
    )
    result = runner.invoke(app, ["invoice", "show", "9999"])
    assert result.exit_code == 1
    assert "Invoice 9999 not found on this account" in result.stderr
    assert "Traceback" not in result.stderr


@respx.mock
def test_show_json_emits_full_detail(seeded_config: Path) -> None:
    detail = _sample_invoice()
    detail["items"] = [
        {
            "id": 1,
            "type": "Hosting",
            "description": "Test item",
            "amount": 10.0,
            "taxed": False,
        }
    ]
    detail["transactions"] = []
    respx.get(f"{BASE}/invoices/12031").mock(
        return_value=httpx.Response(200, json=_detail_envelope(detail)),
    )
    result = runner.invoke(app, ["invoice", "show", "12031", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["id"] == 12031
    assert parsed["status"] == "Paid"
    assert isinstance(parsed["items"], list) and len(parsed["items"]) == 1
    assert parsed["items"][0]["amount"] == 10.0


def test_invoice_with_no_contexts_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["invoice", "list"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr
