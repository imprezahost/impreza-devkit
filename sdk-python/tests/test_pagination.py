"""Unit tests for the internal pagination helpers."""

from __future__ import annotations

from typing import Any

from impreza._pagination import iter_all, iter_pages


def _make_paged_fetcher(pages: list[list[Any]]) -> Any:
    """Return a fetch_page function backed by a fixed list of pages."""

    total = len(pages)

    def fetch(page: int) -> dict[str, Any]:
        idx = page - 1
        data = pages[idx] if 0 <= idx < total else []
        return {
            "success": True,
            "data": data,
            "meta": {
                "request_id": f"req_p{page}",
                "pagination": {
                    "total": sum(len(p) for p in pages),
                    "page": page,
                    "per_page": max((len(p) for p in pages), default=0),
                    "total_pages": total,
                },
            },
        }

    return fetch


def test_iter_pages_walks_until_total_pages() -> None:
    fetch = _make_paged_fetcher([[1, 2], [3, 4], [5]])
    pages = list(iter_pages(fetch))
    assert len(pages) == 3
    assert pages[0]["data"] == [1, 2]
    assert pages[2]["data"] == [5]


def test_iter_all_flattens_items() -> None:
    fetch = _make_paged_fetcher([[1, 2], [3, 4], [5]])
    items = list(iter_all(fetch))
    assert items == [1, 2, 3, 4, 5]


def test_iter_pages_stops_on_missing_pagination_meta() -> None:
    """A response without ``meta.pagination`` is treated as single-page."""

    def fetch(page: int) -> dict[str, Any]:
        return {"success": True, "data": [page], "meta": {"request_id": "req_x"}}

    pages = list(iter_pages(fetch))
    assert len(pages) == 1


def test_iter_all_empty_when_no_data() -> None:
    fetch = _make_paged_fetcher([[]])
    assert list(iter_all(fetch)) == []
