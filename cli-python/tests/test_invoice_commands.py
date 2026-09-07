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


# ── invoice pay ─────────────────────────────────────────────────────


def _pay_envelope() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "invoice_id": 2345,
            "amount": 17.0,
            "currency": "USD",
            "message": "Invoice paid from account balance.",
        },
        "meta": {"request_id": "req_t"},
    }


def _pay_detail_envelope(status: str = "Unpaid") -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": 2345,
            "invoice_num": "2345",
            "date": "2026-03-01",
            "total": 17.0,
            "status": status,
        },
        "meta": {"request_id": "req_t"},
    }


def _err(code: str, message: str) -> dict[str, object]:
    return {
        "success": False,
        "error": {"code": code, "message": message},
        "meta": {"request_id": "req_t"},
    }


@respx.mock
def test_pay_with_yes_skips_fetch_and_prompt(seeded_config: Path) -> None:
    """--yes is the script path: one POST, no confirmation, no extra GET."""
    get_route = respx.get(f"{BASE}/invoices/2345").mock(
        return_value=httpx.Response(200, json=_pay_detail_envelope())
    )
    pay_route = respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=_pay_envelope())
    )
    result = runner.invoke(app, ["invoice", "pay", "2345", "--yes"])
    assert result.exit_code == 0, result.stdout
    assert pay_route.called
    assert not get_route.called
    assert "17.00" in result.stdout


@respx.mock
def test_pay_prompts_with_amount_and_declining_does_not_pay(
    seeded_config: Path,
) -> None:
    """Declining is not an error, and nothing is charged."""
    respx.get(f"{BASE}/invoices/2345").mock(
        return_value=httpx.Response(200, json=_pay_detail_envelope())
    )
    pay_route = respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=_pay_envelope())
    )
    result = runner.invoke(app, ["invoice", "pay", "2345"], input="n\n")
    assert result.exit_code == 0, result.stdout
    assert not pay_route.called
    assert "17.00" in result.stdout
    assert "Cancelled." in result.stdout


@respx.mock
def test_pay_confirmed_charges(seeded_config: Path) -> None:
    respx.get(f"{BASE}/invoices/2345").mock(
        return_value=httpx.Response(200, json=_pay_detail_envelope())
    )
    pay_route = respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=_pay_envelope())
    )
    result = runner.invoke(app, ["invoice", "pay", "2345"], input="y\n")
    assert result.exit_code == 0, result.stdout
    assert pay_route.called


@respx.mock
def test_pay_already_paid_is_not_an_error(seeded_config: Path) -> None:
    """An already-paid invoice is a no-op, not a failure — exit 0 so a
    retrying script doesn't treat it as a broken run."""
    respx.get(f"{BASE}/invoices/2345").mock(
        return_value=httpx.Response(200, json=_pay_detail_envelope(status="Paid"))
    )
    pay_route = respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=_pay_envelope())
    )
    result = runner.invoke(app, ["invoice", "pay", "2345"])
    assert result.exit_code == 0, result.stdout
    assert not pay_route.called
    assert "already paid" in result.stdout.lower()


@respx.mock
def test_pay_409_already_paid_exits_zero(seeded_config: Path) -> None:
    """Same outcome when the race is lost server-side (409 ALREADY_PAID)."""
    respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(409, json=_err("ALREADY_PAID", "Already paid."))
    )
    result = runner.invoke(app, ["invoice", "pay", "2345", "--yes"])
    assert result.exit_code == 0, result.stdout
    assert "already paid" in result.stdout.lower()


@respx.mock
def test_pay_409_insufficient_balance_points_at_topup(seeded_config: Path) -> None:
    respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(
            409,
            json=_err("INSUFFICIENT_BALANCE", "Insufficient balance. Required: USD 17.00"),
        )
    )
    result = runner.invoke(app, ["invoice", "pay", "2345", "--yes"])
    assert result.exit_code == 1
    assert "topup" in result.stdout.lower() or "topup" in result.stderr.lower()


@respx.mock
def test_pay_404(seeded_config: Path) -> None:
    respx.post(f"{BASE}/invoices/999/pay").mock(
        return_value=httpx.Response(404, json=_err("NOT_FOUND", "No such invoice."))
    )
    result = runner.invoke(app, ["invoice", "pay", "999", "--yes"])
    assert result.exit_code == 1
    assert "not found" in result.stderr.lower()


@respx.mock
def test_pay_json_output(seeded_config: Path) -> None:
    respx.post(f"{BASE}/invoices/2345/pay").mock(
        return_value=httpx.Response(200, json=_pay_envelope())
    )
    result = runner.invoke(app, ["invoice", "pay", "2345", "--yes", "-o", "json"])
    assert result.exit_code == 0, result.stdout
    assert json.loads(result.stdout)["invoice_id"] == 2345
