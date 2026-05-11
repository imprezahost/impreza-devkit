"""Shared pytest fixtures for CLI tests.

Every CLI test needs an isolated config file so the test run never
touches the developer's real `~/.config/impreza/config.toml`. The
``isolated_config`` fixture sets ``IMPREZA_CONFIG`` to a per-test
temp path and tears it down after.
"""

from __future__ import annotations

from collections.abc import Iterator
from pathlib import Path

import pytest


@pytest.fixture
def isolated_config(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Iterator[Path]:
    """Yield the path of a temp config file and point ``IMPREZA_CONFIG``
    at it for the test's duration."""
    config_path = tmp_path / "config.toml"
    monkeypatch.setenv("IMPREZA_CONFIG", str(config_path))
    yield config_path
    # Cleanup is automatic — tmp_path is removed by pytest.
