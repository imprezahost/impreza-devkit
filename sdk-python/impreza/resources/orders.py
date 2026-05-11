"""Orders resource — accessed via ``Client.orders`` and ``AsyncClient.orders``.

Phase 1.4d wraps the four order endpoints from the public API:

* ``GET /orders`` → list of recent orders (most recent first).
* ``GET /orders/{id}`` → order detail with line items.
* ``POST /orders`` → create a new order, paid from account balance.
* ``POST /orders/{id}/upgrade`` → upgrade an existing service to a
  different product or billing cycle.

The interesting design choice is how the SDK accepts ``config_options``
and ``custom_fields`` on :meth:`OrdersResource.create`. the API expects
ID-keyed dicts (e.g. ``{3: 5, 4: 10}`` where 3 and 4 are config-option
IDs and 5 / 10 are sub-option IDs). That works for power users who
already know the IDs, but is opaque for everyone else.

This resource accepts **either** ID-keyed dicts (no resolution, fastest)
**or** name-keyed dicts (resolved against the catalog). The two styles
also mix freely:

.. code-block:: python

    # Pure IDs — no extra round-trip:
    c.orders.create(product_id=12, billing_cycle="annually",
                    config_options={3: 5, 4: 10},
                    custom_fields={1: "myserver.example.com"})

    # Pure names — one extra GET /products/{id} for resolution:
    c.orders.create(product_id=12, billing_cycle="annually",
                    config_options={"Disk Space": "20 GB",
                                    "Memory": "2 GB"},
                    custom_fields={"Hostname": "myserver.example.com"})

    # Mixed — both keys and values can be names or IDs independently:
    c.orders.create(product_id=12, billing_cycle="annually",
                    config_options={3: "20 GB"})

When name resolution is needed the resource fetches the product detail
once and caches it on the call. If the same product is ordered again
in a subsequent call, a second fetch is needed (caching across calls
is intentionally not done — the catalog can change).

Resolution failures raise :class:`~impreza.exceptions.InvalidRequest`
(``code = "UNKNOWN_OPTION"`` / ``"UNKNOWN_FIELD"``) before any HTTP
call to ``/orders`` is made.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from ..exceptions import InvalidRequest
from ..models.order import Order, OrderDetail, OrderItem, OrderResult
from ..models.product import ConfigOption, CustomField, ProductDetail

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient


_VALID_CYCLES = {
    "monthly",
    "quarterly",
    "semiannually",
    "annually",
    "biennially",
    "triennially",
}


# ── extractors / body builders (shared) ────────────────────────────────


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _extract_orders(payload: dict[str, object]) -> list[Order]:
    data = _data(payload)
    raw = data.get("orders")
    if not isinstance(raw, list):
        return []
    return [Order.model_validate(item) for item in raw if isinstance(item, dict)]


def _extract_order_detail(payload: dict[str, object]) -> OrderDetail:
    return OrderDetail.model_validate(_data(payload))


def _extract_order_result(payload: dict[str, object]) -> OrderResult:
    return OrderResult.model_validate(_data(payload))


def _extract_product_detail(payload: dict[str, object]) -> ProductDetail:
    return ProductDetail.model_validate(_data(payload))


def _list_params(status: str | None) -> dict[str, object] | None:
    if status is None:
        return None
    return {"status": status}


# ── name → id resolution ───────────────────────────────────────────────


def _resolve_config_options(
    raw: dict[int | str, int | str] | None,
    detail: ProductDetail | None,
) -> dict[int, int]:
    """Convert a user-supplied config_options dict to the {int: int} shape the API wants.

    Accepts keys and values as either ints (already-resolved IDs) or
    strings (option / sub-option names). Pure-ID dicts pass through
    without needing ``detail``; any name lookup requires ``detail``.

    Raises :class:`InvalidRequest` with ``code="UNKNOWN_OPTION"`` for
    any name not found in the product's config options.
    """
    if not raw:
        return {}

    needs_detail = any(isinstance(k, str) or isinstance(v, str) for k, v in raw.items())
    if needs_detail and detail is None:
        raise InvalidRequest(
            "config_options uses name-keyed entries but the product detail "
            "could not be loaded. Pass IDs instead, or contact support if "
            "the product appears to be missing.",
            code="UNKNOWN_OPTION",
            status_code=400,
        )

    by_name: dict[str, ConfigOption] = (
        {opt.name: opt for opt in detail.config_options} if detail else {}
    )
    by_id: dict[int, ConfigOption] = (
        {opt.id: opt for opt in detail.config_options} if detail else {}
    )

    resolved: dict[int, int] = {}
    for key, value in raw.items():
        opt = _resolve_option_key(key, by_name, by_id)
        choice_id = _resolve_option_value(value, opt)
        resolved[opt.id] = choice_id
    return resolved


def _resolve_option_key(
    key: int | str,
    by_name: dict[str, ConfigOption],
    by_id: dict[int, ConfigOption],
) -> ConfigOption:
    if isinstance(key, int):
        opt = by_id.get(key)
        if opt is None and by_id:
            raise InvalidRequest(
                f"Unknown config_options key: {key} (not on this product).",
                code="UNKNOWN_OPTION",
                status_code=400,
            )
        # If we don't have the catalog (caller passed pure-int dict and we
        # never fetched detail), we trust the key blindly.
        return opt or ConfigOption(id=key, name="", type=0, options=[])
    opt = by_name.get(key)
    if opt is None:
        raise InvalidRequest(
            f"Unknown config_options name: {key!r}.",
            code="UNKNOWN_OPTION",
            status_code=400,
        )
    return opt


def _resolve_option_value(value: int | str, opt: ConfigOption) -> int:
    if isinstance(value, int):
        return value  # already an ID — trust it
    # name → id within this option's choices
    for choice in opt.options:
        if choice.name == value:
            return choice.id
    valid = ", ".join(repr(c.name) for c in opt.options) if opt.options else "(no choices)"
    raise InvalidRequest(
        f"Unknown choice {value!r} for config option {opt.name!r}. Valid: {valid}.",
        code="UNKNOWN_OPTION",
        status_code=400,
    )


def _resolve_custom_fields(
    raw: dict[int | str, str] | None,
    detail: ProductDetail | None,
) -> dict[int, str]:
    """Convert a user-supplied custom_fields dict to the {int: str} shape the API wants.

    Same name-resolution policy as :func:`_resolve_config_options`:
    string keys are resolved against the product's custom-field names;
    int keys pass through.
    """
    if not raw:
        return {}

    needs_detail = any(isinstance(k, str) for k in raw)
    if needs_detail and detail is None:
        raise InvalidRequest(
            "custom_fields uses name-keyed entries but the product detail "
            "could not be loaded. Pass IDs instead.",
            code="UNKNOWN_FIELD",
            status_code=400,
        )

    by_name: dict[str, CustomField] = (
        {f.name: f for f in detail.custom_fields} if detail else {}
    )
    by_id: dict[int, CustomField] = (
        {f.id: f for f in detail.custom_fields} if detail else {}
    )

    resolved: dict[int, str] = {}
    for key, value in raw.items():
        if isinstance(key, int):
            if by_id and key not in by_id:
                raise InvalidRequest(
                    f"Unknown custom_fields key: {key} (not on this product).",
                    code="UNKNOWN_FIELD",
                    status_code=400,
                )
            resolved[key] = str(value)
            continue
        field = by_name.get(key)
        if field is None:
            raise InvalidRequest(
                f"Unknown custom_fields name: {key!r}.",
                code="UNKNOWN_FIELD",
                status_code=400,
            )
        resolved[field.id] = str(value)
    return resolved


def _create_body(
    *,
    product_id: int,
    billing_cycle: str,
    domain: str | None,
    hostname: str | None,
    config_options: dict[int, int],
    custom_fields: dict[int, str],
) -> dict[str, object]:
    if billing_cycle not in _VALID_CYCLES:
        raise ValueError(
            f"billing_cycle must be one of {sorted(_VALID_CYCLES)}; got {billing_cycle!r}",
        )
    body: dict[str, object] = {
        "product_id": product_id,
        "billing_cycle": billing_cycle,
    }
    if domain is not None:
        body["domain"] = domain
    if hostname is not None:
        body["hostname"] = hostname
    if config_options:
        # The API wants string-keyed dicts in JSON; the IDs become strings on the wire
        body["config_options"] = {str(k): v for k, v in config_options.items()}
    if custom_fields:
        body["custom_fields"] = {str(k): v for k, v in custom_fields.items()}
    return body


def _upgrade_body(
    *,
    service_id: int,
    new_product_id: int,
    billing_cycle: str,
) -> dict[str, object]:
    if billing_cycle not in _VALID_CYCLES:
        raise ValueError(
            f"billing_cycle must be one of {sorted(_VALID_CYCLES)}; got {billing_cycle!r}",
        )
    return {
        "service_id": service_id,
        "new_product_id": new_product_id,
        "billing_cycle": billing_cycle,
    }


def _needs_resolution(
    config_options: dict[int | str, int | str] | None,
    custom_fields: dict[int | str, str] | None,
) -> bool:
    if config_options:
        for k, v in config_options.items():
            if isinstance(k, str) or isinstance(v, str):
                return True
    if custom_fields:
        for k in custom_fields:
            if isinstance(k, str):
                return True
    return False


# ── sync ───────────────────────────────────────────────────────────────


class OrdersResource:
    """Sync orders — list, get, create, upgrade."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self, *, status: str | None = None) -> list[Order]:
        """Return up to the 50 most recent orders, optionally filtered by status.

        ``status`` accepts these values: ``Pending``, ``Active``, ``Cancelled``,
        ``Fraud``, etc.
        """
        return _extract_orders(self._http.get("/orders", params=_list_params(status)))

    def get(self, order_id: int) -> OrderDetail:
        """Return one order by ID, with its line items."""
        return _extract_order_detail(self._http.get(f"/orders/{order_id}"))

    def create(
        self,
        *,
        product_id: int,
        billing_cycle: str,
        domain: str | None = None,
        hostname: str | None = None,
        config_options: dict[int | str, int | str] | None = None,
        custom_fields: dict[int | str, str] | None = None,
    ) -> OrderResult:
        """Create a new order paid from the client's credit balance.

        ``config_options`` and ``custom_fields`` accept either ID-keyed
        or name-keyed dicts (or a mix). See module docstring for examples.
        """
        detail: ProductDetail | None = None
        if _needs_resolution(config_options, custom_fields):
            detail = _extract_product_detail(self._http.get(f"/products/{product_id}"))

        resolved_options = _resolve_config_options(config_options, detail)
        resolved_fields = _resolve_custom_fields(custom_fields, detail)

        return _extract_order_result(
            self._http.post(
                "/orders",
                json=_create_body(
                    product_id=product_id,
                    billing_cycle=billing_cycle,
                    domain=domain,
                    hostname=hostname,
                    config_options=resolved_options,
                    custom_fields=resolved_fields,
                ),
            )
        )

    def upgrade(
        self,
        *,
        service_id: int,
        new_product_id: int,
        billing_cycle: str,
    ) -> OrderResult:
        """Upgrade an existing service to a new product / billing cycle.

        Charges the prorated difference from the client's credit balance.
        Note: ``config_options`` / ``custom_fields`` are not yet supported
        on upgrade by the upstream API.
        """
        return _extract_order_result(
            self._http.post(
                f"/orders/{service_id}/upgrade",
                json=_upgrade_body(
                    service_id=service_id,
                    new_product_id=new_product_id,
                    billing_cycle=billing_cycle,
                ),
            )
        )


# ── async ──────────────────────────────────────────────────────────────


class AsyncOrdersResource:
    """Async counterpart to :class:`OrdersResource`."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self, *, status: str | None = None) -> list[Order]:
        return _extract_orders(
            await self._http.get("/orders", params=_list_params(status))
        )

    async def get(self, order_id: int) -> OrderDetail:
        return _extract_order_detail(await self._http.get(f"/orders/{order_id}"))

    async def create(
        self,
        *,
        product_id: int,
        billing_cycle: str,
        domain: str | None = None,
        hostname: str | None = None,
        config_options: dict[int | str, int | str] | None = None,
        custom_fields: dict[int | str, str] | None = None,
    ) -> OrderResult:
        detail: ProductDetail | None = None
        if _needs_resolution(config_options, custom_fields):
            detail = _extract_product_detail(
                await self._http.get(f"/products/{product_id}")
            )

        resolved_options = _resolve_config_options(config_options, detail)
        resolved_fields = _resolve_custom_fields(custom_fields, detail)

        return _extract_order_result(
            await self._http.post(
                "/orders",
                json=_create_body(
                    product_id=product_id,
                    billing_cycle=billing_cycle,
                    domain=domain,
                    hostname=hostname,
                    config_options=resolved_options,
                    custom_fields=resolved_fields,
                ),
            )
        )

    async def upgrade(
        self,
        *,
        service_id: int,
        new_product_id: int,
        billing_cycle: str,
    ) -> OrderResult:
        return _extract_order_result(
            await self._http.post(
                f"/orders/{service_id}/upgrade",
                json=_upgrade_body(
                    service_id=service_id,
                    new_product_id=new_product_id,
                    billing_cycle=billing_cycle,
                ),
            )
        )


# OrderItem is re-exported for convenience — callers iterating
# ``order.items`` get type-checked attribute access.
__all__ = [
    "AsyncOrdersResource",
    "Order",
    "OrderDetail",
    "OrderItem",
    "OrderResult",
    "OrdersResource",
]
