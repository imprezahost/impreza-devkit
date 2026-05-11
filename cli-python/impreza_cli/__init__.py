"""Impreza Host CLI — official command-line interface for the
Impreza Host public REST API.

Multi-context machinery (config file at
``$XDG_CONFIG_HOME/impreza/config.toml`` on Linux/macOS,
``%APPDATA%\\impreza\\config.toml`` on Windows) plus the
``impreza context`` subcommand surface and 65 verbs across 11
command groups.

Built on top of :mod:`impreza` (the SDK), so the network layer,
auth, retry, and error handling are inherited.
"""

# Read the installed package version from metadata. Always matches
# `pip show impreza-cli`, so `impreza --version` can't drift from
# the wheel even if someone forgets to bump a hard-coded string.
from importlib.metadata import PackageNotFoundError, version as _pkg_version

try:
    __version__ = _pkg_version("impreza-cli")
except PackageNotFoundError:  # source checkout without an install
    __version__ = "0.0.0+unknown"

del _pkg_version, PackageNotFoundError
