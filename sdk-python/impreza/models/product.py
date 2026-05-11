"""Product catalog response models.

The Impreza catalog returns products with billing-cycle-keyed pricing.
``Product`` is the entry-level shape returned by the listing endpoint;
``ProductDetail`` extends it with ``custom_fields`` and ``config_options``
needed when placing an order. The latter are typed in 1.4d so SDK
callers can resolve options by name instead of the API's internal IDs.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field


class CyclePrice(BaseModel):
    """Price for a single billing cycle."""

    model_config = ConfigDict(extra="ignore")

    price: float
    setup_fee: float = 0.0


class CustomField(BaseModel):
    """A custom field on a product (e.g. ``Hostname`` for VPS orders).

    ``type`` mirrors the field type (``text``, ``dropdown``,
    ``password``, ``link``, ``tickbox``, etc.). For dropdown / radio
    types, ``options`` lists the valid string values. ``required``
    is true if the field must be filled when ordering.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    name: str
    type: str
    description: str | None = None
    options: list[str] = Field(default_factory=list)
    required: bool = False


class ConfigOptionChoice(BaseModel):
    """A single sub-option (sometimes called a "value") within a configurable option.

    For example, a "Disk Space" config option might have choices "10 GB",
    "20 GB", "50 GB" — each with its own ID and per-cycle pricing.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    name: str
    pricing: dict[str, float] = Field(default_factory=dict)


class ConfigOption(BaseModel):
    """A configurable option attached to a product.

    ``type`` is the optiontype (1=dropdown, 2=radio, 3=yes/no,
    4=quantity). For types 1 and 2, ``options`` lists the available
    choices; for 3 and 4 the structure is simpler and may have
    fewer / no entries.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    name: str
    type: int
    options: list[ConfigOptionChoice] = Field(default_factory=list)


class Product(BaseModel):
    """A product (hosting plan, VPS, dedicated server, etc.) in the catalog.

    The ``pricing`` dict is keyed by billing cycle (``monthly``, ``quarterly``,
    ``annually``, etc.) — keys vary per product. Use ``product.pricing.get(cycle)``
    to read a specific cycle.
    """

    model_config = ConfigDict(extra="ignore")

    id: int
    name: str
    description: str | None = None
    type: str
    group: str | None = None
    group_id: int | None = None
    currency: str
    pricing: dict[str, CyclePrice] = Field(default_factory=dict)


class ProductDetail(Product):
    """Full product detail — adds custom fields and configurable options.

    Only returned by ``GET /products/{id}``; the listing endpoint returns
    bare :class:`Product` instances without these extras. The order
    resource (``c.orders.create``) consumes these to resolve human-readable
    option names back to the IDs the API expects.
    """

    model_config = ConfigDict(extra="ignore")

    custom_fields: list[CustomField] = Field(default_factory=list)
    config_options: list[ConfigOption] = Field(default_factory=list)


class ProductGroup(BaseModel):
    """A product group (category) shown by ``GET /products/groups``."""

    model_config = ConfigDict(extra="ignore")

    id: int
    name: str
    product_count: int = 0
