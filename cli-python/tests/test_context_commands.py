"""End-to-end tests for the ``impreza context`` subcommand.

Uses Typer's CliRunner for exit code + stdout/stderr inspection.
The ``isolated_config`` fixture (see conftest.py) sets
``IMPREZA_CONFIG`` to a per-test temp file, so these tests never
touch the developer's real config.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from impreza_cli.config import Config
from impreza_cli.main import app

# Click 8.2+ removed `mix_stderr` from CliRunner — stderr is always
# captured separately now via result.stderr.
runner = CliRunner()

_FAKE_KEY = "imp_" + ("a" * 40)
_FAKE_SECRET = "0" * 64


# ── version + help ────────────────────────────────────────────────────


def test_version_flag_prints_version() -> None:
    result = runner.invoke(app, ["--version"])
    assert result.exit_code == 0
    assert "impreza-cli" in result.stdout


def test_root_help_lists_context_subcommand() -> None:
    result = runner.invoke(app, ["--help"])
    assert result.exit_code == 0
    assert "context" in result.stdout


# ── create ────────────────────────────────────────────────────────────


def test_create_first_context_sets_default(isolated_config: Path) -> None:
    result = runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    assert result.exit_code == 0
    assert "set as default" in result.stdout

    cfg = Config.load(isolated_config)
    assert cfg.default_context == "personal"
    assert cfg.contexts["personal"].api_key == _FAKE_KEY


def test_create_second_context_does_not_change_default(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    result = runner.invoke(
        app,
        ["context", "create", "work", "--key", "imp_" + "b" * 40, "--secret", "1" * 64],
    )
    assert result.exit_code == 0
    assert "set as default" not in result.stdout

    cfg = Config.load(isolated_config)
    assert cfg.default_context == "personal"


def test_create_invalid_name_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(
        app,
        ["context", "create", "name with spaces", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    assert result.exit_code == 1
    assert "Error:" in result.stderr
    assert "illegal character" in result.stderr


def test_create_duplicate_without_overwrite_exits_nonzero(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    result = runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    assert result.exit_code == 1
    assert "already exists" in result.stderr


def test_create_with_overwrite_replaces(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    new_key = "imp_" + "z" * 40
    result = runner.invoke(
        app,
        [
            "context", "create", "personal",
            "--key", new_key, "--secret", _FAKE_SECRET,
            "--overwrite",
        ],
    )
    assert result.exit_code == 0
    cfg = Config.load(isolated_config)
    assert cfg.contexts["personal"].api_key == new_key


def test_create_with_base_url_and_default_output(isolated_config: Path) -> None:
    result = runner.invoke(
        app,
        [
            "context", "create", "personal",
            "--key", _FAKE_KEY, "--secret", _FAKE_SECRET,
            "--base-url", "https://api.example.com/v1",
            "--default-output", "json",
        ],
    )
    assert result.exit_code == 0
    cfg = Config.load(isolated_config)
    ctx = cfg.contexts["personal"]
    assert ctx.base_url == "https://api.example.com/v1"
    assert ctx.default_output == "json"


# ── use ───────────────────────────────────────────────────────────────


def test_use_switches_default(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    runner.invoke(
        app,
        ["context", "create", "work", "--key", "imp_" + "b" * 40, "--secret", "1" * 64],
    )
    result = runner.invoke(app, ["context", "use", "work"])
    assert result.exit_code == 0
    assert "Now using context 'work'" in result.stdout

    cfg = Config.load(isolated_config)
    assert cfg.default_context == "work"


def test_use_unknown_context_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["context", "use", "ghost"])
    assert result.exit_code == 1
    assert "does not exist" in result.stderr


# ── list ──────────────────────────────────────────────────────────────


def test_list_when_empty(isolated_config: Path) -> None:
    result = runner.invoke(app, ["context", "list"])
    assert result.exit_code == 0
    assert "No contexts configured" in result.stdout


def test_list_renders_table(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    runner.invoke(
        app,
        ["context", "create", "work", "--key", "imp_" + "b" * 40, "--secret", "1" * 64],
    )
    result = runner.invoke(app, ["context", "list"])
    assert result.exit_code == 0
    assert "personal" in result.stdout
    assert "work" in result.stdout
    # default flag rendered as 'yes' / 'no'
    assert "yes" in result.stdout


def test_list_json_output_emits_full_keys(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    result = runner.invoke(app, ["context", "list", "--output", "json"])
    assert result.exit_code == 0

    parsed = json.loads(result.stdout)
    assert isinstance(parsed, list)
    assert len(parsed) == 1
    assert parsed[0]["name"] == "personal"
    # JSON output is unmasked so it pipes into jq cleanly
    assert parsed[0]["api_key"] == _FAKE_KEY
    assert parsed[0]["default"] is True


# ── current ───────────────────────────────────────────────────────────


def test_current_with_no_contexts_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["context", "current"])
    assert result.exit_code == 1
    assert "No contexts configured" in result.stderr


def test_current_renders_active_context(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    result = runner.invoke(app, ["context", "current"])
    assert result.exit_code == 0
    assert "personal" in result.stdout


def test_current_json_output(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    result = runner.invoke(app, ["context", "current", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["name"] == "personal"
    assert parsed["api_key"] == _FAKE_KEY


# ── delete ────────────────────────────────────────────────────────────


def test_delete_with_yes_skips_prompt(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    runner.invoke(
        app,
        ["context", "create", "work", "--key", "imp_" + "b" * 40, "--secret", "1" * 64],
    )
    result = runner.invoke(app, ["context", "delete", "work", "--yes"])
    assert result.exit_code == 0
    assert "deleted" in result.stdout

    cfg = Config.load(isolated_config)
    assert cfg.list_contexts() == ["personal"]


def test_delete_unknown_context_exits_nonzero(isolated_config: Path) -> None:
    result = runner.invoke(app, ["context", "delete", "ghost", "--yes"])
    assert result.exit_code == 1
    assert "does not exist" in result.stderr


def test_delete_default_context_clears_default(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    runner.invoke(
        app,
        ["context", "create", "work", "--key", "imp_" + "b" * 40, "--secret", "1" * 64],
    )
    result = runner.invoke(app, ["context", "delete", "personal", "--yes"])
    assert result.exit_code == 0
    assert "no default context now" in result.stdout

    cfg = Config.load(isolated_config)
    assert cfg.default_context is None
    assert cfg.list_contexts() == ["work"]


def test_delete_prompts_when_no_yes_flag(isolated_config: Path) -> None:
    runner.invoke(
        app,
        ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
    )
    # Decline the prompt
    result = runner.invoke(app, ["context", "delete", "personal"], input="n\n")
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout

    cfg = Config.load(isolated_config)
    assert cfg.list_contexts() == ["personal"]


# ── full lifecycle ────────────────────────────────────────────────────


@pytest.mark.parametrize("output_fmt", ["table", "json"])
def test_full_lifecycle(isolated_config: Path, output_fmt: str) -> None:
    """Create two, switch default, delete one, verify state via list."""
    assert (
        runner.invoke(
            app,
            ["context", "create", "personal", "--key", _FAKE_KEY, "--secret", _FAKE_SECRET],
        ).exit_code == 0
    )
    assert (
        runner.invoke(
            app,
            ["context", "create", "work", "--key", "imp_" + "b" * 40, "--secret", "1" * 64],
        ).exit_code == 0
    )
    assert runner.invoke(app, ["context", "use", "work"]).exit_code == 0
    assert runner.invoke(app, ["context", "delete", "personal", "--yes"]).exit_code == 0

    if output_fmt == "json":
        result = runner.invoke(app, ["context", "list", "--output", "json"])
        assert result.exit_code == 0
        parsed = json.loads(result.stdout)
        assert len(parsed) == 1
        assert parsed[0]["name"] == "work"
        assert parsed[0]["default"] is True
    else:
        result = runner.invoke(app, ["context", "list"])
        assert result.exit_code == 0
        assert "work" in result.stdout
        assert "personal" not in result.stdout
