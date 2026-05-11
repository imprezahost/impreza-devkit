"""Shared pytest fixtures."""

from __future__ import annotations

import os
from collections.abc import Iterator

import pytest

from impreza import Client


@pytest.fixture
def live_client() -> Iterator[Client]:
    """Yield a Client built from environment credentials.

    Tests requesting this fixture are integration tests against the real
    ``api.imprezahost.com``. They are skipped automatically when
    ``IMPREZA_API_KEY`` and ``IMPREZA_API_SECRET`` are not set, so the
    suite stays runnable on CI runners without secrets configured.
    """
    if not os.environ.get("IMPREZA_API_KEY") or not os.environ.get("IMPREZA_API_SECRET"):
        pytest.skip(
            "IMPREZA_API_KEY/IMPREZA_API_SECRET not set — skipping live integration test"
        )

    client = Client.from_env()
    try:
        yield client
    finally:
        client.close()
