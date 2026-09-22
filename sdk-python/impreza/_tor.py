"""Tor proxy resolution — internal.

Both :class:`Client` and :class:`AsyncClient` accept three Tor-related
inputs:

* ``proxy=``   — explicit proxy URL (any scheme httpx understands).
* ``use_tor=`` — when ``True``, route through the default Tor SOCKS5
  port at ``127.0.0.1:9050``. Equivalent to setting
  ``IMPREZA_USE_TOR=1`` in the environment.
* ``auto_tor=`` — when ``True``, probe the default Tor port and use it
  if reachable; otherwise fall back to clearnet (no proxy).

``resolve_proxy()`` collapses these inputs into a single proxy URL or
``None``. The first explicit signal wins:

1. ``proxy=`` (caller knows best)
2. ``use_tor=`` or ``IMPREZA_USE_TOR=1``
3. ``auto_tor=`` (probed)

The TCP probe uses a half-second timeout and never raises — failure
just means "Tor not available, use clearnet".
"""

from __future__ import annotations

import os
import re
import socket
from urllib.parse import urlsplit

DEFAULT_TOR_HOST = "127.0.0.1"
DEFAULT_TOR_PORT = 9050
DEFAULT_TOR_PROXY = f"socks5://{DEFAULT_TOR_HOST}:{DEFAULT_TOR_PORT}"
TOR_ENV_VAR = "IMPREZA_USE_TOR"
TOR_PROBE_TIMEOUT = 0.5


def validate_onion_transport(base_url: str, proxy: str | None) -> str | None:
    """Refuse direct onion DNS/transport, including an unavailable auto-Tor fallback."""
    try:
        target = urlsplit(base_url)
        host = (target.hostname or "").lower()
        if not host.rstrip(".").endswith(".onion"):
            return proxy
        onion_pattern = r"(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z2-7]{56}\.onion"
        if (
            re.fullmatch(onion_pattern, host) is None
            or target.scheme not in {"http", "https"}
            or target.username is not None
            or target.password is not None
        ):
            raise ValueError
        route = urlsplit(proxy or "")
        if (
            route.scheme not in {"socks5", "socks5h"}
            or not route.hostname
            or not route.port
            or route.username is not None
            or route.password is not None
            or route.path not in {"", "/"}
            or route.query
            or route.fragment
        ):
            raise ValueError
        # httpx SOCKS passes destination names to the SOCKS server. Normalize
        # the alias for versions of httpx that only recognize socks5.
        return route._replace(scheme="socks5").geturl()
    except ValueError:
        raise ValueError(
            "A valid v3 onion URL requires an explicit SOCKS5 proxy or use_tor=True"
        ) from None


def is_tor_available(host: str = DEFAULT_TOR_HOST, port: int = DEFAULT_TOR_PORT) -> bool:
    """Quick TCP probe to check whether Tor SOCKS5 is reachable.

    Returns ``True`` if a connection to ``host:port`` succeeds within
    :data:`TOR_PROBE_TIMEOUT` seconds, ``False`` on any failure.
    """
    try:
        with socket.create_connection((host, port), timeout=TOR_PROBE_TIMEOUT):
            return True
    except OSError:
        return False


def env_use_tor() -> bool:
    """Return ``True`` when ``IMPREZA_USE_TOR`` is set to a truthy value.

    Truthy: ``1``, ``true``, ``yes``, ``on`` (case-insensitive).
    """
    raw = os.environ.get(TOR_ENV_VAR)
    if raw is None:
        return False
    return raw.strip().lower() in {"1", "true", "yes", "on"}


def resolve_proxy(
    proxy: str | None = None,
    *,
    use_tor: bool = False,
    auto_tor: bool = False,
) -> str | None:
    """Pick the proxy URL to hand to httpx.

    Precedence (first match wins):

    1. ``proxy`` — explicit caller intent.
    2. ``use_tor`` flag or ``IMPREZA_USE_TOR=1`` env — force Tor.
    3. ``auto_tor`` — probe Tor; use it if reachable, else clearnet.
    4. None — clearnet.
    """
    if proxy:
        return proxy
    if use_tor or env_use_tor():
        return DEFAULT_TOR_PROXY
    if auto_tor and is_tor_available():
        return DEFAULT_TOR_PROXY
    return None
