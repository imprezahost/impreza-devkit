"""Unit tests for impreza_cli.config — Config load/save and the
context CRUD on the in-memory representation. No CLI / Typer
involvement here; that's covered in test_context_commands.py.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from impreza_cli.config import (
    Config,
    Context,
    ContextAlreadyExists,
    ContextNotFound,
    InvalidContextName,
    NoActiveContext,
    NoContextsConfigured,
    default_config_path,
)

# A real-shape API key + secret pair (44 + 64 chars). Using fixtures
# that match production length means the mask helper actually runs
# its non-trivial code path in tests.
_FAKE_KEY = "imp_" + ("a" * 40)
_FAKE_SECRET = "0" * 64


# ── default_config_path ───────────────────────────────────────────────


def test_default_path_honours_override(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    target = tmp_path / "custom.toml"
    monkeypatch.setenv("IMPREZA_CONFIG", str(target))
    assert default_config_path() == target.resolve()


def test_default_path_falls_back_to_app_dir(monkeypatch: pytest.MonkeyPatch) -> None:
    """Without override, the path lives under click.get_app_dir."""
    monkeypatch.delenv("IMPREZA_CONFIG", raising=False)
    p = default_config_path()
    assert p.name == "config.toml"
    assert "impreza" in str(p).lower()


# ── Config.load on missing file ───────────────────────────────────────


def test_load_returns_empty_config_when_file_missing(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "absent.toml")
    assert cfg.contexts == {}
    assert cfg.default_context is None
    assert cfg.path == tmp_path / "absent.toml"


# ── add_context, get_context, list_contexts ───────────────────────────


def test_add_first_context_sets_it_as_default(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    ctx = cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    assert isinstance(ctx, Context)
    assert ctx.name == "personal"
    assert cfg.default_context == "personal"
    assert cfg.list_contexts() == ["personal"]


def test_second_context_does_not_steal_default(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.add_context("work", api_key="imp_" + "b" * 40, api_secret="1" * 64)
    assert cfg.default_context == "personal"
    assert cfg.list_contexts() == ["personal", "work"]


def test_add_existing_name_without_overwrite_raises(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    with pytest.raises(ContextAlreadyExists):
        cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)


def test_add_existing_name_with_overwrite_replaces(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    new_key = "imp_" + "z" * 40
    cfg.add_context(
        "personal",
        api_key=new_key,
        api_secret=_FAKE_SECRET,
        overwrite=True,
    )
    assert cfg.contexts["personal"].api_key == new_key


@pytest.mark.parametrize(
    "bad_name",
    ["", " ", "name with spaces", "weird@name", "name/slash", "x" * 51],
)
def test_invalid_name_rejected(tmp_path: Path, bad_name: str) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    with pytest.raises(InvalidContextName):
        cfg.add_context(bad_name, api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)


@pytest.mark.parametrize(
    "good_name",
    ["personal", "work-1", "ci_runner", "abc", "x" * 50, "Test123"],
)
def test_valid_names_accepted(tmp_path: Path, good_name: str) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    ctx = cfg.add_context(good_name, api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    assert ctx.name == good_name


# ── use_context ───────────────────────────────────────────────────────


def test_use_context_switches_default(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.add_context("work", api_key="imp_" + "b" * 40, api_secret="1" * 64)
    cfg.use_context("work")
    assert cfg.default_context == "work"


def test_use_unknown_context_raises(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    with pytest.raises(ContextNotFound):
        cfg.use_context("ghost")


# ── remove_context ────────────────────────────────────────────────────


def test_remove_context_clears_default_when_removing_default(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.add_context("work", api_key="imp_" + "b" * 40, api_secret="1" * 64)
    assert cfg.default_context == "personal"
    cfg.remove_context("personal")
    assert cfg.default_context is None
    assert cfg.list_contexts() == ["work"]


def test_remove_context_keeps_default_when_removing_other(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.add_context("work", api_key="imp_" + "b" * 40, api_secret="1" * 64)
    cfg.remove_context("work")
    assert cfg.default_context == "personal"
    assert cfg.list_contexts() == ["personal"]


def test_remove_unknown_context_raises(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    with pytest.raises(ContextNotFound):
        cfg.remove_context("ghost")


# ── get_context resolution ────────────────────────────────────────────


def test_get_context_with_no_contexts_raises(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    with pytest.raises(NoContextsConfigured):
        cfg.get_context()


def test_get_context_with_no_default_set_raises(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.default_context = None
    with pytest.raises(NoActiveContext):
        cfg.get_context()


def test_get_context_by_name(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.add_context("work", api_key="imp_" + "b" * 40, api_secret="1" * 64)
    ctx = cfg.get_context("work")
    assert ctx.name == "work"


def test_get_context_unknown_name_raises(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    with pytest.raises(ContextNotFound):
        cfg.get_context("ghost")


# ── round-trip via save / load ────────────────────────────────────────


def test_save_then_load_round_trips(tmp_path: Path) -> None:
    cfg = Config.load(tmp_path / "c.toml")
    cfg.add_context(
        "personal",
        api_key=_FAKE_KEY,
        api_secret=_FAKE_SECRET,
        base_url="https://api.example.com/v1",
        default_output="json",
    )
    cfg.add_context(
        "work",
        api_key="imp_" + "b" * 40,
        api_secret="1" * 64,
    )
    cfg.use_context("work")
    cfg.save()

    reloaded = Config.load(tmp_path / "c.toml")
    assert reloaded.default_context == "work"
    assert reloaded.list_contexts() == ["personal", "work"]
    p = reloaded.contexts["personal"]
    assert p.api_key == _FAKE_KEY
    assert p.api_secret == _FAKE_SECRET
    assert p.base_url == "https://api.example.com/v1"
    assert p.default_output == "json"
    w = reloaded.contexts["work"]
    assert w.base_url is None
    assert w.default_output is None


def test_save_creates_parent_directory(tmp_path: Path) -> None:
    nested = tmp_path / "deeply" / "nested" / "config.toml"
    cfg = Config.load(nested)
    cfg.add_context("only", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()
    assert nested.is_file()


def test_load_skips_malformed_context_entries(tmp_path: Path) -> None:
    """A context missing required fields shouldn't break the whole
    load — the well-formed siblings should still come through."""
    target = tmp_path / "c.toml"
    target.write_text(
        'default_context = "good"\n'
        "\n"
        "[contexts.good]\n"
        f'api_key = "{_FAKE_KEY}"\n'
        f'api_secret = "{_FAKE_SECRET}"\n'
        "\n"
        "[contexts.broken]\n"
        'api_key = "imp_no_secret_here"\n'
    )
    cfg = Config.load(target)
    assert cfg.list_contexts() == ["good"]
    assert cfg.default_context == "good"
