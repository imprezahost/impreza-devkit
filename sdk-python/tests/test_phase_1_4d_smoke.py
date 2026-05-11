"""Live integration smoke tests for Phase 1.4d (orders).

Read-only operations only. Mutating endpoints (`create`, `upgrade`)
are covered by mocks — running them against the live API would
charge real money against the test account's balance.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_4d_smoke.py -v -s
"""

from __future__ import annotations

import pytest

from impreza import Client, Order, OrderDetail


def test_smoke_orders_list_round_trips(live_client: Client) -> None:
    """``c.orders.list()`` returns a (possibly empty) list of Order models."""
    orders = live_client.orders.list()
    assert isinstance(orders, list)
    for o in orders:
        assert isinstance(o, Order)
        assert o.id > 0
    print(f"\n  account has {len(orders)} order(s) on file")


def test_smoke_orders_get_first_order_returns_detail(live_client: Client) -> None:
    """If the account has any order, fetch its detail and verify it
    decodes into OrderDetail with line items."""
    orders = live_client.orders.list()
    if not orders:
        pytest.skip("no orders on this account — nothing to fetch")

    detail = live_client.orders.get(orders[0].id)
    assert isinstance(detail, OrderDetail)
    assert detail.id == orders[0].id
    print(f"\n  order {detail.id}: {len(detail.items)} line item(s); status={detail.status!r}")


def test_smoke_catalog_returns_typed_options_and_fields(live_client: Client) -> None:
    """Verify the typed ConfigOption + CustomField models decode against
    a real product. We pick the first product the catalog returns."""
    products = live_client.catalog.products()
    if not products:
        pytest.skip("catalog is empty — nothing to inspect")

    detail = live_client.catalog.product(products[0].id)
    print(
        f"\n  product {detail.id} ({detail.name}): "
        f"{len(detail.config_options)} config option(s), "
        f"{len(detail.custom_fields)} custom field(s)"
    )
    for opt in detail.config_options:
        # type field is the optiontype int; ensure it decoded
        assert isinstance(opt.id, int) and opt.name and isinstance(opt.type, int)
        for choice in opt.options:
            assert isinstance(choice.id, int) and choice.name
    for field in detail.custom_fields:
        assert isinstance(field.id, int) and field.name and field.type
