"""Unit tests for ``c.catalog`` (sync + async).

Mocked via ``respx`` — no real API call.
"""

from __future__ import annotations

import httpx
import pytest
import respx

from impreza import AsyncClient, Client, Product, ProductDetail, ProductGroup, TldPricing

BASE = "https://api.imprezahost.com/v1"


def _products_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "products": [
                {
                    "id": 15,
                    "name": "VPS Plan 2",
                    "description": "4 vCPU, 8GB RAM, 100GB SSD",
                    "type": "server",
                    "group": "VPS Hosting",
                    "group_id": 3,
                    "currency": "USD",
                    "pricing": {
                        "monthly": {"price": 15.00, "setup_fee": 0.00},
                        "annually": {"price": 144.00, "setup_fee": 0.00},
                    },
                },
            ],
            "total": 1,
            "currency": "USD",
        },
        "meta": {"request_id": "req_test"},
    }


def _product_detail_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "id": 15,
            "name": "VPS Plan 2",
            "description": "4 vCPU, 8GB RAM, 100GB SSD",
            "type": "server",
            "group": "VPS Hosting",
            "group_id": 3,
            "currency": "USD",
            "pricing": {"monthly": {"price": 15.00, "setup_fee": 0.0}},
            "custom_fields": [{"id": 1, "name": "Hostname", "type": "text", "required": True}],
            "config_options": [],
        },
        "meta": {"request_id": "req_test"},
    }


def _groups_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "groups": [
                {"id": 1, "name": "Shared Hosting", "product_count": 4},
                {"id": 3, "name": "VPS Hosting", "product_count": 6},
            ],
            "total": 2,
        },
        "meta": {"request_id": "req_test"},
    }


def _tlds_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "tlds": [
                {
                    "tld": ".com",
                    "register": {"1": 12.99, "2": 25.98},
                    "renew": {"1": 14.99},
                    "currency": "USD",
                    "min_years": 1,
                    "cheapest": 12.99,
                },
                {
                    "tld": ".net",
                    "register": {"1": 14.99},
                    "renew": {"1": 16.99},
                    "currency": "USD",
                    "min_years": 1,
                    "cheapest": 14.99,
                },
            ],
            "total": 2,
            "currency": "USD",
        },
        "meta": {"request_id": "req_test"},
    }


@respx.mock
def test_catalog_products_returns_list() -> None:
    respx.get(f"{BASE}/products").mock(return_value=httpx.Response(200, json=_products_payload()))

    with Client(api_key="x", api_secret="y") as c:
        products = c.catalog.products()

    assert len(products) == 1
    assert isinstance(products[0], Product)
    assert products[0].id == 15
    assert products[0].pricing["monthly"].price == 15.0
    assert products[0].pricing["annually"].price == 144.0


@respx.mock
def test_catalog_products_sends_filters() -> None:
    route = respx.get(f"{BASE}/products").mock(
        return_value=httpx.Response(200, json=_products_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.catalog.products(group="VPS", type="server")

    sent = route.calls.last.request
    assert sent.url.params.get("group") == "VPS"
    assert sent.url.params.get("type") == "server"


@respx.mock
def test_catalog_product_detail_includes_custom_fields() -> None:
    respx.get(f"{BASE}/products/15").mock(
        return_value=httpx.Response(200, json=_product_detail_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        product = c.catalog.product(15)

    assert isinstance(product, ProductDetail)
    assert product.id == 15
    # Phase 1.4d: custom_fields/config_options are now typed Pydantic models,
    # not raw dicts. Attribute access replaces subscript.
    assert len(product.custom_fields) == 1
    assert product.custom_fields[0].name == "Hostname"
    assert product.custom_fields[0].required is True


@respx.mock
def test_catalog_product_groups() -> None:
    respx.get(f"{BASE}/products/groups").mock(
        return_value=httpx.Response(200, json=_groups_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        groups = c.catalog.product_groups()

    assert len(groups) == 2
    assert all(isinstance(g, ProductGroup) for g in groups)
    assert groups[1].name == "VPS Hosting"
    assert groups[1].product_count == 6


@respx.mock
def test_catalog_tlds_parses_pricing_dicts() -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        tlds = c.catalog.tlds()

    assert len(tlds) == 2
    assert isinstance(tlds[0], TldPricing)
    assert tlds[0].tld == ".com"
    assert tlds[0].register_prices["1"] == 12.99
    assert tlds[0].register_prices["2"] == 25.98
    assert tlds[0].renew_prices["1"] == 14.99
    assert tlds[0].cheapest == 12.99


@respx.mock
def test_catalog_tlds_sends_filter_param() -> None:
    route = respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.catalog.tlds(filter=".com,.net")

    sent = route.calls.last.request
    assert sent.url.params.get("tld") == ".com,.net"


@pytest.mark.asyncio
@respx.mock
async def test_async_catalog_products() -> None:
    respx.get(f"{BASE}/products").mock(return_value=httpx.Response(200, json=_products_payload()))

    async with AsyncClient(api_key="x", api_secret="y") as c:
        products = await c.catalog.products()

    assert products[0].id == 15
    assert products[0].pricing["monthly"].price == 15.0


@pytest.mark.asyncio
@respx.mock
async def test_async_catalog_tlds() -> None:
    respx.get(f"{BASE}/domains/pricing").mock(
        return_value=httpx.Response(200, json=_tlds_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        tlds = await c.catalog.tlds(filter=".com")

    assert tlds[0].tld == ".com"


@pytest.mark.asyncio
@respx.mock
async def test_async_catalog_product_detail() -> None:
    """Async parity for ``c.catalog.product(id)`` — same envelope
    extraction as the sync test, just under the async client."""
    respx.get(f"{BASE}/products/15").mock(
        return_value=httpx.Response(200, json=_product_detail_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        product = await c.catalog.product(15)

    assert isinstance(product, ProductDetail)
    assert product.id == 15
    assert len(product.custom_fields) == 1
    assert product.custom_fields[0].name == "Hostname"


@pytest.mark.asyncio
@respx.mock
async def test_async_catalog_product_groups() -> None:
    """Async parity for ``c.catalog.product_groups()``."""
    respx.get(f"{BASE}/products/groups").mock(
        return_value=httpx.Response(200, json=_groups_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        groups = await c.catalog.product_groups()

    assert len(groups) >= 1
    assert groups[0].name
