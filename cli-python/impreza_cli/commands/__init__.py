"""Subcommand modules — one per resource group.

Each module exposes an ``app`` (a ``typer.Typer`` instance) that
``impreza_cli.main`` mounts under the right command name. Phase 2.1
ships :mod:`.context`. Subsequent fases add ``account``, ``catalog``,
``domain``, ``vps``, ``invoice``, ``key``, etc.
"""
