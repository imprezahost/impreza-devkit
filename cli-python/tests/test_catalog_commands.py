"""Unit tests for ``impreza catalog`` commands.

respx-mocked HTTP, isolated config (via the ``isolated_config``
fixture from conftest.py), assertions on exit code + stdout/stderr.

Live integration smoke for these commands lives in
test_phase_2_3_smoke.py.
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


def _products_envelope(products: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"products": products, "total": len(products)},
        "meta": {"request_id": "req_t"},
    }


def _groups_envelope(groups: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"groups": groups, "total": len(groups)},
        "meta": {"request_id": "req_t"},
    }


def _tlds_envelope(tlds: list[dict[str, object]]) -> dict[str, object]:
    return {
        "success": True,
        "data": {"tlds": tlds, "total": len(tlds)},
        "meta": {"request_id": "req_t"},
    }


def _sample_product(
    pid: int = 571,
    name: str = "USA Linux Hosting I",
    group: str = "Hosting",
    type_: str = "hostingaccount",
) -> dict[str, object]:
    return {
        "id": pid,
        "name": name,
        "description": "A simple hosting plan.",
        "type": type_,
        "group": group,
        "group_id": 5,
        "currency": "USD",
        "pricing": {
            "monthly": {"price": 5.0, "setup_fee": 0.0},
            "annually": {"price": 50.0, "setup_fee": 0.0},
        },
    }


def _sample_tld(tld: str = ".com", cheapest: float = 12.99) -> dict[str, object]:
    return {
        "tld": tld,
        "register": {"1": cheapest, "2": cheapest * 2},
        "renew": {"1": cheapest * 1.1},
        "currency": "USD",
        "min_years": 1,
        "cheapest": cheapest,
    }


# ── catalog products ─────────────────────────────────────────────────


@respx.mock
def test_products_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(
            200,
            json=_products_envelope([_sample_product(), _sample_product(572, "VPS I")]),
        )
    )
    result = runner.invoke(app, ["catalog", "products"])
    assert result.exit_code == 0, result.stderr
    assert "571" in result.stdout
    assert "572" in result.stdout
    # Cheapest cycle rendered in the dedicated column
    assert "monthly: 5.00" in result.stdout


@respx.mock
def test_products_empty_default_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(200, json=_products_envelope([])),
    )
    result = runner.invoke(app, ["catalog", "products"])
    assert result.exit_code == 0
    assert "No products in the catalog yet" in result.stdout


@respx.mock
def test_products_empty_with_filter_mentions_filter(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(200, json=_products_envelope([])),
    )
    result = runner.invoke(app, ["catalog", "products", "--group", "VPS"])
    assert result.exit_code == 0
    assert "No products match the filter" in result.stdout
    assert "group='VPS'" in result.stdout


@respx.mock
def test_products_filter_passes_params_to_api(seeded_config: Path) -> None:
    route = respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(200, json=_products_envelope([])),
    )
    result = runner.invoke(
        app,
        ["catalog", "products", "--group", "VPS", "--type", "server"],
    )
    assert result.exit_code == 0
    url = str(route.calls.last.request.url)
    assert "group=VPS" in url
    assert "type=server" in url


@respx.mock
def test_products_json_emits_full_pricing_dict(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(
            200, json=_products_envelope([_sample_product()])
        ),
    )
    result = runner.invoke(app, ["catalog", "products", "--output", "json"])
    assert result.exit_code == 0, result.stderr
    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list) and len(parsed) == 1
    p = parsed[0]
    assert p["id"] == 571
    # Full per-cycle pricing dict preserved for caller-driven cycle pick
    assert p["pricing"]["monthly"]["price"] == 5.0
    assert p["pricing"]["annually"]["price"] == 50.0


@respx.mock
def test_products_no_pricing_renders_dash(seeded_config: Path) -> None:
    """Edge case: a product with no pricing configured renders the
    cheapest-cycle column as ``-`` rather than crashing."""
    p = _sample_product()
    p["pricing"] = {}
    respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(200, json=_products_envelope([p])),
    )
    result = runner.invoke(app, ["catalog", "products"])
    assert result.exit_code == 0
    # The "no pricing" sentinel
    assert "-" in result.stdout


# ── catalog product-groups ───────────────────────────────────────────


@respx.mock
def test_product_groups_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products/groups").mock(
        return_value=httpx.Response(
            200,
            json=_groups_envelope(
                [
                    {"id": 1, "name": "Hosting", "product_count": 12},
                    {"id": 2, "name": "VPS Server", "product_count": 8},
                ]
            ),
        )
    )
    result = runner.invoke(app, ["catalog", "product-groups"])
    assert result.exit_code == 0, result.stderr
    assert "Hosting" in result.stdout
    assert "VPS Server" in result.stdout
    assert "12" in result.stdout
    assert "8" in result.stdout


@respx.mock
def test_product_groups_empty_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products/groups").mock(
        return_value=httpx.Response(200, json=_groups_envelope([])),
    )
    result = runner.invoke(app, ["catalog", "product-groups"])
    assert result.exit_code == 0
    assert "No product groups defined yet" in result.stdout


@respx.mock
def test_product_groups_json_output(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products/groups").mock(
        return_value=httpx.Response(
            200,
            json=_groups_envelope(
                [{"id": 1, "name": "Hosting", "product_count": 12}]
            ),
        )
    )
    result = runner.invoke(
        app, ["catalog", "product-groups", "--output", "json"]
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed[0]["id"] == 1
    assert parsed[0]["product_count"] == 12


# ── catalog tlds ─────────────────────────────────────────────────────


@respx.mock
def test_tlds_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(
            200,
            json=_tlds_envelope([_sample_tld(".com", 12.99), _sample_tld(".net", 14.50)]),
        )
    )
    result = runner.invoke(app, ["catalog", "tlds"])
    assert result.exit_code == 0, result.stderr
    assert ".com" in result.stdout
    assert ".net" in result.stdout
    assert "12.99" in result.stdout
    assert "14.50" in result.stdout


@respx.mock
def test_tlds_filter_passes_to_api(seeded_config: Path) -> None:
    route = respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_envelope([])),
    )
    result = runner.invoke(app, ["catalog", "tlds", "--filter", ".com,.net"])
    assert result.exit_code == 0
    url = str(route.calls.last.request.url)
    # The SDK forwards `filter` as the `tld` query param
    assert "tld=" in url


@respx.mock
def test_tlds_empty_with_filter_mentions_filter(seeded_config: Path) -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_envelope([])),
    )
    result = runner.invoke(app, ["catalog", "tlds", "--filter", ".xyz"])
    assert result.exit_code == 0
    assert "No TLDs match the filter" in result.stdout
    assert "'.xyz'" in result.stdout


@respx.mock
def test_tlds_no_1_year_price_renders_dash(seeded_config: Path) -> None:
    """A TLD that requires a 2-year minimum has no `1` key in
    register_prices — must render `-` rather than crash."""
    t = _sample_tld(".de", 8.99)
    t["register"] = {"2": 17.98}
    t["renew"] = {"2": 19.78}
    t["min_years"] = 2
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_envelope([t])),
    )
    result = runner.invoke(app, ["catalog", "tlds"])
    assert result.exit_code == 0
    # The dash for missing 1-year + min_years=2 visible somewhere
    assert ".de" in result.stdout


@respx.mock
def test_tlds_json_emits_full_pricing(seeded_config: Path) -> None:
    """JSON output preserves the per-year pricing dicts so callers
    can pick a multi-year register cycle."""
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(
            200, json=_tlds_envelope([_sample_tld(".com", 12.99)])
        ),
    )
    result = runner.invoke(app, ["catalog", "tlds", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed[0]["tld"] == ".com"
    # JSON dump uses by_alias=True — JSON keys are register / renew (the
    # API's wire names), not the internal Python register_prices /
    # renew_prices.
    assert parsed[0]["register"]["1"] == 12.99
    assert parsed[0]["register"]["2"] == 25.98


# ── error paths shared across catalog ────────────────────────────────


def test_catalog_with_no_contexts_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["catalog", "products"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr


@respx.mock
def test_catalog_api_error_renders_friendly_message(seeded_config: Path) -> None:
    respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(
            500,
            json={
                "success": False,
                "meta": {"request_id": "req_oops"},
                "error": {
                    "code": "INTERNAL_ERROR",
                    "message": "Catalog cache rebuild in progress.",
                },
            },
        ),
    )
    result = runner.invoke(app, ["catalog", "products"])
    assert result.exit_code == 1
    assert "Catalog cache rebuild" in result.stderr
    assert "INTERNAL_ERROR" in result.stderr
    assert "req_oops" in result.stderr
    assert "Traceback" not in result.stderr
