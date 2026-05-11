"""Live integration smoke tests for Phase 1.3 read-only resources.

Hits the real ``api.imprezahost.com``. Skipped when credentials are not
in the environment so CI without secrets stays green.

Run locally::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_3_smoke.py -v -s
"""

from __future__ import annotations

from impreza import Client, Invoice, Product, Service, TldPricing


def test_smoke_services_list(live_client: Client) -> None:
    """``c.account.services.list()`` returns owned services."""
    services = live_client.account.services.list()

    assert isinstance(services, list)
    for svc in services:
        assert isinstance(svc, Service)
        assert svc.id > 0
        assert svc.status

    if services:
        first = services[0]
        print(f"\n  first service: id={first.id} product={first.product} status={first.status}")
    else:
        print("\n  account has no services")


def test_smoke_catalog_products(live_client: Client) -> None:
    """``c.catalog.products()`` returns at least one product."""
    products = live_client.catalog.products()

    assert isinstance(products, list)
    assert len(products) > 0, "production catalog should have at least one product"
    for p in products:
        assert isinstance(p, Product)
        assert p.id > 0
        assert p.currency

    print(f"\n  catalog: {len(products)} products, first: {products[0].name}")


def test_smoke_catalog_tlds(live_client: Client) -> None:
    """``c.catalog.tlds(filter='.com')`` returns .com pricing."""
    tlds = live_client.catalog.tlds(filter=".com")

    assert isinstance(tlds, list)
    com = next((t for t in tlds if t.tld == ".com"), None)
    assert com is not None, ".com TLD should be in the catalog"
    assert isinstance(com, TldPricing)
    assert com.cheapest is not None and com.cheapest > 0
    assert com.currency

    print(f"\n  .com cheapest: {com.cheapest} {com.currency}")


def test_smoke_invoices_list(live_client: Client) -> None:
    """``c.invoices.list()`` parses without error (list may be empty)."""
    invoices = live_client.invoices.list()

    assert isinstance(invoices, list)
    for inv in invoices:
        assert isinstance(inv, Invoice)
        assert inv.id > 0
        assert inv.invoice_num

    print(f"\n  invoices: {len(invoices)} total")
