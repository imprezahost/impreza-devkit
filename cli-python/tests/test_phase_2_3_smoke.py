"""Live integration smoke for Phase 2.3 (`impreza catalog *`).

Skips silently when ``IMPREZA_API_KEY`` / ``IMPREZA_API_SECRET`` are
not set. Catalog endpoints are read-only and account-scoped — running
this against a real account never mutates state, so no destructive
flag is needed.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_2_3_smoke.py -v -s
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest
from typer.testing import CliRunner

from impreza_cli.config import Config
from impreza_cli.main import app

runner = CliRunner()


def _live_creds() -> tuple[str, str]:
    api_key = os.environ.get("IMPREZA_API_KEY", "")
    api_secret = os.environ.get("IMPREZA_API_SECRET", "")
    if not api_key or not api_secret:
        pytest.skip("IMPREZA_API_KEY / IMPREZA_API_SECRET not set")
    return api_key, api_secret


@pytest.fixture
def live_seeded_config(isolated_config: Path) -> Path:
    api_key, api_secret = _live_creds()
    cfg = Config.load(isolated_config)
    cfg.add_context("live", api_key=api_key, api_secret=api_secret)
    cfg.save()
    return isolated_config


def test_smoke_catalog_products_returns_typed_rows(
    live_seeded_config: Path,
) -> None:
    """The catalog should have at least a handful of products on
    a real install. We don't assert on count — just shape."""
    result = runner.invoke(app, ["catalog", "products", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    assert len(parsed) > 0, "expected at least one product in the catalog"
    for p in parsed[:5]:
        assert isinstance(p["id"], int)
        assert isinstance(p["name"], str) and p["name"]
        assert isinstance(p["currency"], str) and len(p["currency"]) == 3
        assert isinstance(p["pricing"], dict)
    print(f"\n  catalog products: {len(parsed)} total")
    for p in parsed[:3]:
        cycles = ", ".join(p["pricing"].keys())
        print(f"    id={p['id']:>4} {p['name']!r:<40} cycles=[{cycles}]")


def test_smoke_catalog_product_groups_returns_typed_rows(
    live_seeded_config: Path,
) -> None:
    result = runner.invoke(app, ["catalog", "product-groups", "--output", "json"])
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    assert len(parsed) > 0, "expected at least one product group on a real install"
    for g in parsed:
        assert isinstance(g["id"], int)
        assert isinstance(g["name"], str)
        assert isinstance(g["product_count"], int)
    print(f"\n  product groups: {len(parsed)} total")
    for g in parsed[:5]:
        print(f"    id={g['id']:>3} {g['name']!r:<30} count={g['product_count']}")


def test_smoke_catalog_tlds_returns_pricing(
    live_seeded_config: Path,
) -> None:
    """Filter to a small TLD set so we don't pull the whole 1000-TLD
    catalog through the wire — the assertion is just that .com is
    present and decodes correctly."""
    result = runner.invoke(
        app,
        ["catalog", "tlds", "--filter", ".com,.net,.org", "--output", "json"],
    )
    assert result.exit_code == 0, result.stderr

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list) and len(parsed) > 0
    by_tld = {t["tld"]: t for t in parsed}
    # .com is universal — every registrar carries it.
    assert ".com" in by_tld
    com = by_tld[".com"]
    assert isinstance(com["currency"], str)
    assert isinstance(com["register"], dict)
    # 1-year register price is the most common offering — should be
    # present on .com everywhere.
    assert "1" in com["register"]
    print(
        f"\n  tlds: {len(parsed)} matched filter '.com,.net,.org'"
    )
    for t in parsed:
        reg_1y = t["register"].get("1", "-")
        print(
            f"    {t['tld']:<8} register-1y={reg_1y} {t['currency']}"
        )
