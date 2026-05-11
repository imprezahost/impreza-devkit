"""TLD (domain) pricing models.

``GET /domains/pricing`` returns per-TLD register/renew price maps
keyed by integer year strings (``"1"``, ``"2"``, ``"3"``).
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field


class TldPricing(BaseModel):
    """Registration and renewal pricing for a single TLD.

    ``register_prices`` and ``renew_prices`` are dicts keyed by
    years-as-string with float prices (e.g. ``{"1": 12.99, "2": 25.98}``).
    The Python attributes carry the ``_prices`` suffix to avoid shadowing
    ``BaseModel.register`` (inherited from ``abc.ABCMeta``); the JSON
    keys remain ``register`` / ``renew`` via Pydantic field aliases, so
    parsing is transparent and ``model_dump(by_alias=True)`` round-trips.
    """

    model_config = ConfigDict(extra="ignore", populate_by_name=True)

    tld: str
    register_prices: dict[str, float] = Field(default_factory=dict, alias="register")
    renew_prices: dict[str, float] = Field(default_factory=dict, alias="renew")
    currency: str
    min_years: int = 1
    cheapest: float | None = None
