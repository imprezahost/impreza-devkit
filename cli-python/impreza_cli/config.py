"""Multi-context configuration store for the CLI.

The CLI reads/writes a single TOML file holding any number of named
contexts (sets of credentials + per-context defaults). This module
hides the on-disk format from the rest of the CLI and exposes a
small, fully-typed surface:

* :class:`Config` — load, save, mutate.
* :func:`default_config_path` — platform-appropriate location.
* :class:`Context` — an individual context's resolved values.
* :class:`ConfigError` (and friends) — typed error hierarchy that
  the command layer maps to user-facing exit codes / messages.

On-disk format (``config.toml``)::

    default_context = "personal"

    [contexts.personal]
    api_key = "imp_..."
    api_secret = "..."
    base_url = "https://api.imprezahost.com/v1"  # optional override
    default_output = "table"                      # optional

    [contexts.work]
    api_key = "imp_..."
    api_secret = "..."

    [settings]
    poll_interval = 2.0    # optional CLI default
    use_tor = false        # optional CLI default

The path is selected by :func:`default_config_path`, which delegates
to ``click.get_app_dir("impreza")``. That follows XDG on Linux,
``%APPDATA%`` on Windows, and ``~/Library/Application Support`` on
macOS (with a fallback to ``~/.config/impreza`` on Linux when
``XDG_CONFIG_HOME`` is not set, matching what most CLIs do).

Permissions: on POSIX, when the file is created the mode is set to
0600 so only the owner can read the credentials. Windows ACLs are
left to the OS default — there is no portable way to lock down a
file to a single user from Python's stdlib without third-party
packages.
"""

from __future__ import annotations

import contextlib
import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import click
import tomli_w

if sys.version_info >= (3, 11):
    import tomllib
else:  # pragma: no cover - exercised on 3.10 runners
    import tomli as tomllib  # type: ignore[import-not-found]


__all__ = [
    "Config",
    "ConfigError",
    "Context",
    "ContextAlreadyExists",
    "ContextNotFound",
    "InvalidContextName",
    "NoActiveContext",
    "NoContextsConfigured",
    "default_config_path",
]


# ── path resolution ───────────────────────────────────────────────────


def default_config_path() -> Path:
    """Return the platform-appropriate config file path.

    ``click.get_app_dir`` follows the OS conventions:

    * **Linux**: ``$XDG_CONFIG_HOME/impreza/`` (defaults to
      ``~/.config/impreza/``)
    * **macOS**: ``~/Library/Application Support/impreza/``
    * **Windows**: ``%APPDATA%\\impreza\\``

    Override with ``IMPREZA_CONFIG`` for tests or non-standard
    layouts. The override is treated as an absolute file path,
    not a directory.
    """
    override = os.environ.get("IMPREZA_CONFIG")
    if override:
        return Path(override).expanduser().resolve()
    return Path(click.get_app_dir("impreza")) / "config.toml"


# ── exceptions ────────────────────────────────────────────────────────


class ConfigError(Exception):
    """Base for config-level errors. The CLI command layer maps these
    to non-zero exit codes with friendly messages."""


class ContextNotFound(ConfigError):
    """Requested context does not exist in the config file."""


class ContextAlreadyExists(ConfigError):
    """``context create`` was called with an existing name without
    ``--overwrite``."""


class InvalidContextName(ConfigError):
    """Context name fails the format check (alphanumeric + ``-`` /
    ``_`` only, 1-50 chars). Hyphens and underscores allowed so
    "my-personal" / "ci_runner" both work; spaces and special
    characters rejected so the name never needs quoting in shell
    commands."""


class NoContextsConfigured(ConfigError):
    """No contexts exist yet. ``impreza context create`` is the
    bootstrap step."""


class NoActiveContext(ConfigError):
    """``default_context`` is unset and no override was provided."""


# ── data classes ──────────────────────────────────────────────────────


@dataclass
class Context:
    """Resolved view of a single context.

    ``name`` is the lookup key; the rest are inputs to the SDK
    :class:`impreza.Client`.
    """

    name: str
    api_key: str
    api_secret: str
    base_url: str | None = None
    default_output: str | None = None

    def to_toml_dict(self) -> dict[str, Any]:
        """Render to the on-disk dict shape (only set keys are
        emitted, so the file stays minimal and round-trip-friendly)."""
        body: dict[str, Any] = {
            "api_key": self.api_key,
            "api_secret": self.api_secret,
        }
        if self.base_url is not None:
            body["base_url"] = self.base_url
        if self.default_output is not None:
            body["default_output"] = self.default_output
        return body


@dataclass
class _Settings:
    """Per-config (not per-context) defaults. Optional in the file."""

    poll_interval: float | None = None
    use_tor: bool | None = None

    def to_toml_dict(self) -> dict[str, Any]:
        body: dict[str, Any] = {}
        if self.poll_interval is not None:
            body["poll_interval"] = self.poll_interval
        if self.use_tor is not None:
            body["use_tor"] = self.use_tor
        return body


# ── name validation ───────────────────────────────────────────────────


_NAME_MAX = 50


def _validate_context_name(name: str) -> None:
    if not name or len(name) > _NAME_MAX:
        raise InvalidContextName(
            f"Context name must be 1-{_NAME_MAX} chars; got: {name!r}"
        )
    for ch in name:
        if not (ch.isalnum() or ch in ("-", "_")):
            raise InvalidContextName(
                f"Context name {name!r} contains illegal character {ch!r}; "
                "only alphanumerics, '-', and '_' are allowed."
            )


# ── Config ────────────────────────────────────────────────────────────


@dataclass
class Config:
    """In-memory representation of ``config.toml``.

    Use :meth:`load` to read from disk (or get an empty config if the
    file does not exist), :meth:`save` to write back, and the various
    mutators below to add / remove / select contexts. The mutators
    raise :class:`ConfigError` subclasses on bad input — they never
    silently swallow.
    """

    path: Path
    default_context: str | None = None
    contexts: dict[str, Context] = field(default_factory=dict)
    settings: _Settings = field(default_factory=_Settings)

    # ── persistence ────────────────────────────────────────────────

    @classmethod
    def load(cls, path: Path | None = None) -> Config:
        """Load the config from ``path`` (or the default location).

        If the file does not exist, return an empty :class:`Config`
        rooted at that path so a subsequent :meth:`save` writes
        there. This is the bootstrap path — first-time users have
        nothing on disk yet.
        """
        resolved = path or default_config_path()
        if not resolved.is_file():
            return cls(path=resolved)

        with resolved.open("rb") as fh:
            raw = tomllib.load(fh)

        contexts_raw = raw.get("contexts", {})
        contexts: dict[str, Context] = {}
        if isinstance(contexts_raw, dict):
            for name, body in contexts_raw.items():
                if not isinstance(body, dict):
                    continue
                # Skip rather than raise on individual malformed
                # entries — the user should still be able to repair
                # one broken context without losing the others.
                api_key = body.get("api_key")
                api_secret = body.get("api_secret")
                if not isinstance(api_key, str) or not isinstance(api_secret, str):
                    continue
                base_url = body.get("base_url")
                default_output = body.get("default_output")
                contexts[str(name)] = Context(
                    name=str(name),
                    api_key=api_key,
                    api_secret=api_secret,
                    base_url=base_url if isinstance(base_url, str) else None,
                    default_output=(
                        default_output if isinstance(default_output, str) else None
                    ),
                )

        settings_raw = raw.get("settings", {})
        settings = _Settings()
        if isinstance(settings_raw, dict):
            poll = settings_raw.get("poll_interval")
            if isinstance(poll, (int, float)):
                settings.poll_interval = float(poll)
            tor = settings_raw.get("use_tor")
            if isinstance(tor, bool):
                settings.use_tor = tor

        default_context = raw.get("default_context")
        if not isinstance(default_context, str):
            default_context = None

        return cls(
            path=resolved,
            default_context=default_context,
            contexts=contexts,
            settings=settings,
        )

    def save(self) -> None:
        """Write the config back to disk atomically.

        Creates the parent directory if missing. On POSIX, sets the
        file mode to ``0600`` after write so that only the owning
        user can read the credentials.
        """
        body: dict[str, Any] = {}
        if self.default_context is not None:
            body["default_context"] = self.default_context
        if self.contexts:
            body["contexts"] = {
                name: ctx.to_toml_dict() for name, ctx in self.contexts.items()
            }
        settings_body = self.settings.to_toml_dict()
        if settings_body:
            body["settings"] = settings_body

        self.path.parent.mkdir(parents=True, exist_ok=True)

        # Write to a sibling temp file then rename so a crash mid-
        # write can't corrupt the live config.
        tmp = self.path.with_suffix(self.path.suffix + ".tmp")
        with tmp.open("wb") as fh:
            tomli_w.dump(body, fh)
        os.replace(tmp, self.path)

        # Lock down on POSIX. Windows ACLs are best-effort via OS
        # default — locking down portably needs a third-party lib.
        # Best-effort chmod: if it fails (network FS, exotic mount)
        # we don't want to hide a successful write behind a
        # permission warning.
        if os.name == "posix":
            with contextlib.suppress(OSError):
                os.chmod(self.path, 0o600)

    # ── mutators ───────────────────────────────────────────────────

    def add_context(
        self,
        name: str,
        *,
        api_key: str,
        api_secret: str,
        base_url: str | None = None,
        default_output: str | None = None,
        overwrite: bool = False,
    ) -> Context:
        """Add (or replace, with ``overwrite=True``) a context.

        If this is the first context, it is also set as the
        ``default_context`` automatically. Returns the new Context
        object.

        Raises:
            InvalidContextName: name fails format check.
            ContextAlreadyExists: name is taken and ``overwrite=False``.
        """
        _validate_context_name(name)
        if name in self.contexts and not overwrite:
            raise ContextAlreadyExists(
                f"Context {name!r} already exists. Pass overwrite=True to replace it."
            )
        ctx = Context(
            name=name,
            api_key=api_key,
            api_secret=api_secret,
            base_url=base_url,
            default_output=default_output,
        )
        first_context = not self.contexts
        self.contexts[name] = ctx
        if first_context or self.default_context is None:
            self.default_context = name
        return ctx

    def remove_context(self, name: str) -> None:
        """Delete a context. If it was the default, clear the default
        (the user is expected to pick a new one explicitly via
        ``context use``)."""
        if name not in self.contexts:
            raise ContextNotFound(f"Context {name!r} does not exist.")
        del self.contexts[name]
        if self.default_context == name:
            self.default_context = None

    def use_context(self, name: str) -> None:
        """Mark a context as the default."""
        if name not in self.contexts:
            raise ContextNotFound(f"Context {name!r} does not exist.")
        self.default_context = name

    # ── readers ────────────────────────────────────────────────────

    def list_contexts(self) -> list[str]:
        """Return all context names, sorted alphabetically."""
        return sorted(self.contexts.keys())

    def get_context(self, name: str | None = None) -> Context:
        """Resolve a context. If ``name`` is None, return the
        ``default_context``.

        Raises:
            NoContextsConfigured: no contexts defined at all.
            NoActiveContext: no name provided and no default set.
            ContextNotFound: name provided but no match.
        """
        if not self.contexts:
            raise NoContextsConfigured(
                "No contexts configured yet. Run "
                "`impreza context create <name> --key ... --secret ...` "
                "to add one."
            )
        if name is None:
            if self.default_context is None:
                raise NoActiveContext(
                    "No default context set. Run "
                    "`impreza context use <name>` to pick one."
                )
            name = self.default_context
        ctx = self.contexts.get(name)
        if ctx is None:
            raise ContextNotFound(f"Context {name!r} does not exist.")
        return ctx
