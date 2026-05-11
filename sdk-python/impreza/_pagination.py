"""Pagination helpers — internal.

The Impreza API paginates list endpoints via ``?page=N&per_page=K`` query
params, with a ``meta.pagination`` block on the response carrying
``total``, ``page``, ``per_page``, and ``total_pages``.

These helpers turn a per-page fetcher (``Callable[[int], dict]``) into an
iterator over pages or items. Resource classes wire them up to their own
list endpoints — this module does not know about specific resources.
"""

from __future__ import annotations

from collections.abc import Callable, Iterator
from typing import Any


def iter_pages(
    fetch_page: Callable[[int], dict[str, Any]],
    *,
    start_page: int = 1,
) -> Iterator[dict[str, Any]]:
    """Yield each raw page payload returned by ``fetch_page(page_number)``.

    Stops when:

    * ``meta.pagination.total_pages`` indicates we have reached the last page, OR
    * the page returns an empty ``data`` list, OR
    * the page has no ``meta.pagination`` block at all (single-page result).
    """

    page = start_page
    while True:
        payload = fetch_page(page)
        yield payload

        meta_raw = payload.get("meta")
        meta: dict[str, Any] = meta_raw if isinstance(meta_raw, dict) else {}
        pagination_raw = meta.get("pagination")
        pagination: dict[str, Any] | None = (
            pagination_raw if isinstance(pagination_raw, dict) else None
        )

        if pagination is None:
            return

        total_pages = pagination.get("total_pages")
        if isinstance(total_pages, int) and page >= total_pages:
            return

        data = payload.get("data")
        if isinstance(data, list) and not data:
            return

        page += 1


def iter_all(
    fetch_page: Callable[[int], dict[str, Any]],
    *,
    start_page: int = 1,
) -> Iterator[Any]:
    """Yield every item across all pages.

    Each page's ``data`` field is expected to be a list; non-list ``data``
    values are skipped (a single-resource endpoint should not be iterated).
    """

    for payload in iter_pages(fetch_page, start_page=start_page):
        data = payload.get("data")
        if isinstance(data, list):
            yield from data
