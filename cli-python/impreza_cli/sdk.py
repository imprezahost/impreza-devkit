"""SDK bootstrap — single source of truth for "given a context, hand
me an :class:`impreza.Client`".

Every command that needs to call the API goes through this module so
the resolution rules (which context, which base URL, Tor or not)
live in one place. Adding new resolution sources later (per-context
``proxy=`` overrides, env-var fallbacks, etc.) is a one-file change.

The SDK's own :func:`Client.from_env` reads ``IMPREZA_API_KEY`` /
``IMPREZA_API_SECRET`` directly from the environment — that path
stays available for users who prefer env-var auth and bypass the
context machinery entirely. This module is for the context-driven
path (the CLI's primary UX).
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import typer
from impreza import Client

from .config import Config, ConfigError
from .output import error
from .state import GlobalState

__all__ = [
    "make_client",
    "make_client_or_exit",
]


def make_client(
    state: GlobalState,
    *,
    config_path: Path | None = None,
) -> Client:
    """Build a sync :class:`impreza.Client` from the active context.

    Args:
        state: The CLI's :class:`~.state.GlobalState`. Reads
            ``state.context_override`` to pick a non-default context.
        config_path: Optional override for the config file location
            (mostly used in tests; production flow always uses the
            default).

    Raises:
        ConfigError: any of the config-resolution errors —
            :class:`~.config.NoContextsConfigured`,
            :class:`~.config.NoActiveContext`,
            :class:`~.config.ContextNotFound`. The :func:`make_client_or_exit`
            wrapper catches these for the typical CLI command flow;
            tests / library callers can let them propagate.

    Returns:
        A sync :class:`Client` ready to call. Caller is responsible
        for closing it (the standard `with Client(...) as c:` pattern
        works — Typer commands just let it close on process exit
        which is fine for short-lived CLI invocations).
    """
    cfg = Config.load(config_path)
    ctx = cfg.get_context(state.context_override)

    kwargs: dict[str, Any] = {
        "api_key": ctx.api_key,
        "api_secret": ctx.api_secret,
    }
    if ctx.base_url:
        kwargs["base_url"] = ctx.base_url

    # Tor preference comes from the [settings] block (CLI-wide). A
    # future per-context override (one VPN context, one clearnet, etc.)
    # would slot in here without touching callers.
    if cfg.settings.use_tor:
        kwargs["use_tor"] = True

    return Client(**kwargs)


def make_client_or_exit(
    state: GlobalState,
    *,
    config_path: Path | None = None,
) -> Client:
    """Same as :func:`make_client`, but converts config errors into
    friendly stderr output and a non-zero exit.

    This is the path every Typer command follows. Bugs (network
    errors at construction time, etc.) still raise so the traceback
    isn't swallowed.
    """
    try:
        return make_client(state, config_path=config_path)
    except ConfigError as exc:
        error(str(exc))
        raise typer.Exit(code=1) from None
