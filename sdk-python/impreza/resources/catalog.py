"""Catalog resource — read-only browse over products, groups, and TLD pricing.

Catalog is a static reference area — values change only when staff edit
Impreza Account, not as a side effect of customer activity. Methods read like
verbs (``c.catalog.products(group="VPS")``) rather than the
``.list()/.get()`` pattern used by managed resources, since there is no
collection lifecycle to mirror.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from ..models.product import Product, ProductDetail, ProductGroup
from ..models.tld import TldPricing

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


# ── extractors (shared sync + async) ───────────────────────────────────


def _extract_products(payload: dict[str, object]) -> list[Product]:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    items_raw = data.get("products")
    items = items_raw if isinstance(items_raw, list) else []
    return [Product.model_validate(item) for item in items]


def _extract_product_detail(payload: dict[str, object]) -> ProductDetail:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    return ProductDetail.model_validate(data)


def _extract_product_groups(payload: dict[str, object]) -> list[ProductGroup]:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    items_raw = data.get("groups")
    items = items_raw if isinstance(items_raw, list) else []
    return [ProductGroup.model_validate(item) for item in items]


def _extract_tlds(payload: dict[str, object]) -> list[TldPricing]:
    data_raw = payload.get("data")
    data = data_raw if isinstance(data_raw, dict) else {}
    items_raw = data.get("tlds")
    items = items_raw if isinstance(items_raw, list) else []
    return [TldPricing.model_validate(item) for item in items]


def _products_params(group: str | None, type: str | None) -> dict[str, object] | None:
    params: dict[str, object] = {}
    if group is not None:
        params["group"] = group
    if type is not None:
        params["type"] = type
    return params or None


def _tlds_params(filter: str | None) -> dict[str, object] | None:
    return {"tld": filter} if filter else None


# ── sync ───────────────────────────────────────────────────────────────


class CatalogResource:
    """Sync access to product catalog, product groups, and TLD pricing."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def products(
        self,
        *,
        group: str | None = None,
        type: str | None = None,
    ) -> list[Product]:
        """List products, optionally filtered by group name or product type.

        ``type`` accepts ``hostingaccount``, ``reselleraccount``, ``server``,
        or ``other``.
        """
        payload = self._http.get("/products", params=_products_params(group, type))
        return _extract_products(payload)

    def product(self, product_id: int) -> ProductDetail:
        """Return full product detail (custom fields + config options) for ordering."""
        payload = self._http.get(f"/products/{product_id}")
        return _extract_product_detail(payload)

    def product_groups(self) -> list[ProductGroup]:
        """List all product groups with the count of products in each."""
        payload = self._http.get("/products/groups")
        return _extract_product_groups(payload)

    def tlds(self, *, filter: str | None = None) -> list[TldPricing]:
        """List TLD pricing.

        ``filter`` accepts a comma-separated list of TLDs (e.g.
        ``".com,.net,.io"``). Without a filter, the full TLD catalog is returned.
        """
        payload = self._http.get("/domains/pricing", params=_tlds_params(filter))
        return _extract_tlds(payload)


# ── async ──────────────────────────────────────────────────────────────


class AsyncCatalogResource:
    """Async access to product catalog, product groups, and TLD pricing."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def products(
        self,
        *,
        group: str | None = None,
        type: str | None = None,
    ) -> list[Product]:
        payload = await self._http.get("/products", params=_products_params(group, type))
        return _extract_products(payload)

    async def product(self, product_id: int) -> ProductDetail:
        payload = await self._http.get(f"/products/{product_id}")
        return _extract_product_detail(payload)

    async def product_groups(self) -> list[ProductGroup]:
        payload = await self._http.get("/products/groups")
        return _extract_product_groups(payload)

    async def tlds(self, *, filter: str | None = None) -> list[TldPricing]:
        payload = await self._http.get("/domains/pricing", params=_tlds_params(filter))
        return _extract_tlds(payload)
