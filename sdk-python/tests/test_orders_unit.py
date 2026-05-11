"""Unit tests for the orders resource (Phase 1.4d).

Covers list / get / create / upgrade with the four input modes for
config_options + custom_fields (no opts, ID-keyed, name-keyed,
mixed) plus the resolution-failure edge cases.

Mocked via respx — no real API call.
"""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from impreza import (
    AsyncClient,
    Client,
    InvalidRequest,
    Order,
    OrderDetail,
    OrderResult,
)

BASE = "https://api.imprezahost.com/v1"


def _ok(data: dict[str, object]) -> dict[str, object]:
    return {"success": True, "data": data, "meta": {"request_id": "req_test"}}


def _wrap_orders(items: list[dict[str, object]]) -> dict[str, object]:
    return _ok({"orders": items, "total": len(items)})


def _product_detail_payload(product_id: int) -> dict[str, object]:
    """A product detail with one config option (Disk Space → 10/20 GB) and
    one custom field (Hostname). Used by the resolution tests."""
    return _ok(
        {
            "id": product_id,
            "name": "VPS Plan 2",
            "type": "vps",
            "group": "VPS Hosting",
            "group_id": 1,
            "currency": "USD",
            "pricing": {"monthly": {"price": 15.0, "setup_fee": 0.0}},
            "custom_fields": [
                {
                    "id": 1,
                    "name": "Hostname",
                    "type": "text",
                    "description": "VM hostname",
                    "options": [],
                    "required": True,
                },
                {
                    "id": 2,
                    "name": "OS",
                    "type": "dropdown",
                    "description": "operating system",
                    "options": ["debian-12", "ubuntu-22"],
                    "required": False,
                },
            ],
            "config_options": [
                {
                    "id": 3,
                    "name": "Disk Space",
                    "type": 1,
                    "options": [
                        {"id": 5, "name": "10 GB", "pricing": {"monthly": 0.0}},
                        {"id": 6, "name": "20 GB", "pricing": {"monthly": 5.0}},
                    ],
                },
                {
                    "id": 4,
                    "name": "Memory",
                    "type": 1,
                    "options": [
                        {"id": 7, "name": "1 GB", "pricing": {"monthly": 0.0}},
                        {"id": 8, "name": "2 GB", "pricing": {"monthly": 5.0}},
                    ],
                },
            ],
        }
    )


def _order_result_payload(order_id: int = 100) -> dict[str, object]:
    return _ok(
        {
            "order_id": order_id,
            "invoice_id": order_id + 1000,
            "product": "VPS Plan 2",
            "amount": 15.0,
            "currency": "USD",
            "status": "Active",
            "message": "ok",
        }
    )


# ── list / get ─────────────────────────────────────────────────────────


@respx.mock
def test_orders_list_returns_typed_orders() -> None:
    respx.get(f"{BASE}/orders").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_orders(
                [
                    {
                        "id": 100,
                        "order_number": "ORD-0001",
                        "date": "2026-05-01",
                        "amount": 15.0,
                        "invoice_id": 1100,
                        "status": "Active",
                        "payment_method": "banktransfer",
                    }
                ]
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        orders = c.orders.list()
    assert len(orders) == 1
    assert isinstance(orders[0], Order)
    assert orders[0].id == 100


@respx.mock
def test_orders_list_with_status_filter_passes_query_param() -> None:
    route = respx.get(f"{BASE}/orders").mock(
        return_value=httpx.Response(200, json=_wrap_orders([]))
    )
    with Client(api_key="x", api_secret="y") as c:
        c.orders.list(status="Pending")
    assert route.called
    assert route.calls.last.request.url.query == b"status=Pending"


@respx.mock
def test_orders_get_returns_detail_with_items() -> None:
    respx.get(f"{BASE}/orders/100").mock(
        return_value=httpx.Response(
            200,
            json=_ok(
                {
                    "id": 100,
                    "order_number": "ORD-0001",
                    "date": "2026-05-01",
                    "amount": 15.0,
                    "invoice_id": 1100,
                    "status": "Active",
                    "payment_method": "banktransfer",
                    "items": [
                        {
                            "service_id": 17988,
                            "domain": "vps.example.com",
                            "product": "VPS Plan 2",
                            "status": "Active",
                            "billing_cycle": "monthly",
                            "amount": 15.0,
                        }
                    ],
                }
            ),
        )
    )
    with Client(api_key="x", api_secret="y") as c:
        order = c.orders.get(100)
    assert isinstance(order, OrderDetail)
    assert len(order.items) == 1
    assert order.items[0].service_id == 17988


# ── create: pure-id mode (no resolution, no extra GET) ────────────────


@respx.mock
def test_create_with_pure_id_options_skips_product_lookup() -> None:
    """When config_options + custom_fields use only int keys/values,
    the SDK skips the product detail GET — one HTTP call total."""
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(201, json=_order_result_payload())
    )
    # If the SDK accidentally hits /products/{id}, this'll error
    # because we never mock the route.
    with Client(api_key="x", api_secret="y") as c:
        result = c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            domain="vps.example.com",
            config_options={3: 6, 4: 8},
            custom_fields={1: "myserver.example.com"},
        )
    assert isinstance(result, OrderResult)
    assert result.order_id == 100
    body = json.loads(create_route.calls.last.request.read())
    # IDs serialized as string keys on the wire (matching the API contract)
    assert body["config_options"] == {"3": 6, "4": 8}
    assert body["custom_fields"] == {"1": "myserver.example.com"}


@respx.mock
def test_create_with_no_options_omits_keys() -> None:
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(201, json=_order_result_payload())
    )
    with Client(api_key="x", api_secret="y") as c:
        c.orders.create(product_id=12, billing_cycle="annually")
    body = json.loads(create_route.calls.last.request.read())
    assert "config_options" not in body
    assert "custom_fields" not in body
    assert body["billing_cycle"] == "annually"


# ── create: name resolution ────────────────────────────────────────────


@respx.mock
def test_create_resolves_names_via_product_detail_lookup() -> None:
    detail_route = respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(200, json=_product_detail_payload(12))
    )
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(201, json=_order_result_payload())
    )
    with Client(api_key="x", api_secret="y") as c:
        c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            config_options={"Disk Space": "20 GB", "Memory": "2 GB"},
            custom_fields={"Hostname": "myserver"},
        )
    assert detail_route.called  # resolution happened
    body = json.loads(create_route.calls.last.request.read())
    # "Disk Space" → id=3, "20 GB" → id=6
    # "Memory" → id=4, "2 GB" → id=8
    assert body["config_options"] == {"3": 6, "4": 8}
    # "Hostname" → id=1
    assert body["custom_fields"] == {"1": "myserver"}


@respx.mock
def test_create_resolves_mixed_name_and_id_keys() -> None:
    """Caller can pass 'Disk Space' as a name and Memory as an int —
    both should resolve correctly. Sub-option values too."""
    respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(200, json=_product_detail_payload(12))
    )
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(201, json=_order_result_payload())
    )
    with Client(api_key="x", api_secret="y") as c:
        c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            config_options={"Disk Space": 6, 4: "2 GB"},
        )
    body = json.loads(create_route.calls.last.request.read())
    assert body["config_options"] == {"3": 6, "4": 8}


@respx.mock
def test_create_unknown_config_option_name_raises_invalid_request() -> None:
    respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(200, json=_product_detail_payload(12))
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(InvalidRequest) as exc_info,
    ):
        c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            config_options={"Bandwidth": "100 GB"},  # not on the product
        )
    assert exc_info.value.code == "UNKNOWN_OPTION"
    assert "Bandwidth" in str(exc_info.value)


@respx.mock
def test_create_unknown_choice_for_known_option_raises() -> None:
    respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(200, json=_product_detail_payload(12))
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(InvalidRequest) as exc_info,
    ):
        c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            config_options={"Disk Space": "999 GB"},
        )
    assert exc_info.value.code == "UNKNOWN_OPTION"
    assert "999 GB" in str(exc_info.value)
    assert "Disk Space" in str(exc_info.value)


@respx.mock
def test_create_unknown_custom_field_name_raises() -> None:
    respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(200, json=_product_detail_payload(12))
    )
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(InvalidRequest) as exc_info,
    ):
        c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            custom_fields={"NonExistent": "value"},
        )
    assert exc_info.value.code == "UNKNOWN_FIELD"


# ── create: validation ─────────────────────────────────────────────────


def test_create_invalid_billing_cycle_raises_value_error_locally() -> None:
    """Bad billing cycle is caught client-side before any HTTP call."""
    with Client(api_key="x", api_secret="y") as c, pytest.raises(ValueError, match="billing_cycle"):
        c.orders.create(product_id=12, billing_cycle="weekly")  # type: ignore[arg-type]


# ── upgrade ────────────────────────────────────────────────────────────


@respx.mock
def test_upgrade_posts_to_service_path() -> None:
    route = respx.post(f"{BASE}/orders/17988/upgrade").mock(
        return_value=httpx.Response(201, json=_order_result_payload(order_id=200))
    )
    with Client(api_key="x", api_secret="y") as c:
        result = c.orders.upgrade(
            service_id=17988, new_product_id=42, billing_cycle="annually"
        )
    assert route.called
    body = json.loads(route.calls.last.request.read())
    assert body["service_id"] == 17988
    assert body["new_product_id"] == 42
    assert body["billing_cycle"] == "annually"
    assert isinstance(result, OrderResult)
    assert result.order_id == 200


# ── async parallel ─────────────────────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_orders_list() -> None:
    respx.get(f"{BASE}/orders").mock(
        return_value=httpx.Response(
            200,
            json=_wrap_orders([{"id": 1, "amount": 10.0, "status": "Active"}]),
        )
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        orders = await c.orders.list()
    assert len(orders) == 1


@pytest.mark.asyncio
@respx.mock
async def test_async_orders_create_with_id_keys() -> None:
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(201, json=_order_result_payload())
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        result = await c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            config_options={3: 6},
        )
    assert isinstance(result, OrderResult)
    body = json.loads(create_route.calls.last.request.read())
    assert body["config_options"] == {"3": 6}


@pytest.mark.asyncio
@respx.mock
async def test_async_orders_create_resolves_names() -> None:
    respx.get(f"{BASE}/products/12").mock(
        return_value=httpx.Response(200, json=_product_detail_payload(12))
    )
    create_route = respx.post(f"{BASE}/orders").mock(
        return_value=httpx.Response(201, json=_order_result_payload())
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        await c.orders.create(
            product_id=12,
            billing_cycle="monthly",
            config_options={"Disk Space": "20 GB"},
        )
    body = json.loads(create_route.calls.last.request.read())
    assert body["config_options"] == {"3": 6}


@pytest.mark.asyncio
@respx.mock
async def test_async_orders_upgrade() -> None:
    route = respx.post(f"{BASE}/orders/17988/upgrade").mock(
        return_value=httpx.Response(201, json=_order_result_payload(order_id=200))
    )
    async with AsyncClient(api_key="x", api_secret="y") as c:
        result = await c.orders.upgrade(
            service_id=17988, new_product_id=42, billing_cycle="annually"
        )
    assert route.called
    assert result.order_id == 200
