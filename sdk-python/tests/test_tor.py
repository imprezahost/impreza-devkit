"""Unit tests for ``impreza._tor`` proxy resolution."""

from __future__ import annotations

from unittest.mock import patch

from impreza._tor import (
    DEFAULT_TOR_PROXY,
    TOR_ENV_VAR,
    env_use_tor,
    is_tor_available,
    resolve_proxy,
)


def test_explicit_proxy_wins_over_use_tor() -> None:
    out = resolve_proxy("http://corp-proxy:3128", use_tor=True, auto_tor=True)
    assert out == "http://corp-proxy:3128"


def test_use_tor_true_returns_default_socks5() -> None:
    assert resolve_proxy(use_tor=True) == DEFAULT_TOR_PROXY


def test_env_var_truthy_acts_like_use_tor() -> None:
    for value in ("1", "true", "TRUE", "yes", "ON"):
        with patch.dict("os.environ", {TOR_ENV_VAR: value}, clear=False):
            assert env_use_tor() is True
            assert resolve_proxy() == DEFAULT_TOR_PROXY


def test_env_var_falsy_returns_no_proxy() -> None:
    for value in ("0", "false", "no", "off", ""):
        with patch.dict("os.environ", {TOR_ENV_VAR: value}, clear=False):
            assert env_use_tor() is False
            assert resolve_proxy() is None


def test_env_var_unset_returns_no_proxy() -> None:
    with patch.dict("os.environ", {}, clear=True):
        assert env_use_tor() is False
        assert resolve_proxy() is None


def test_auto_tor_uses_tor_when_available() -> None:
    with (
        patch.dict("os.environ", {}, clear=True),
        patch("impreza._tor.is_tor_available", return_value=True),
    ):
        assert resolve_proxy(auto_tor=True) == DEFAULT_TOR_PROXY


def test_auto_tor_falls_back_to_clearnet_when_unavailable() -> None:
    with (
        patch.dict("os.environ", {}, clear=True),
        patch("impreza._tor.is_tor_available", return_value=False),
    ):
        assert resolve_proxy(auto_tor=True) is None


def test_no_signals_returns_none() -> None:
    with patch.dict("os.environ", {}, clear=True):
        assert resolve_proxy() is None


def test_is_tor_available_returns_bool_no_raise() -> None:
    """The probe must never raise — failure means Tor isn't running, that's it."""
    # Use a port we know nothing listens on.
    assert is_tor_available(host="127.0.0.1", port=1) is False
