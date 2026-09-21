"""Unit tests for ``impreza config`` commands (P4 config-as-code CLI).

respx mocks the SDK's HTTP layer end-to-end: export writes the exact
document text, apply runs the prepare → review → apply flow carrying the
exact digest, and plan renders a saved review. The confirmation path,
digest mismatch refusals and the no-changes rendering are all covered.
"""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest
import respx
from typer.testing import CliRunner

from impreza_cli.config import Config
from impreza_cli.main import app

runner = CliRunner()

BASE = "https://api.imprezahost.com/v1"
_FAKE_KEY = "imp_" + ("a" * 40)
_FAKE_SECRET = "0" * 64

_DEPLOYMENT = "dpl_" + "c" * 16
_PLAN = "cplan_" + "6" * 24
_DIGEST = "f" * 64
_DOCUMENT = {
    "schema": 1,
    "deployment": {"name": "web", "build_strategy": "static_files"},
    "resources": {},
}


@pytest.fixture
def seeded_config(isolated_config: Path) -> Path:
    cfg = Config.load(isolated_config)
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()
    return isolated_config


def _export_envelope() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "schema": 1,
            "deployment_id": _DEPLOYMENT,
            "name": "web",
            "exported_at": "2026-09-21T00:00:00Z",
            "document": _DOCUMENT,
            "document_text": json.dumps(_DOCUMENT, indent=2) + "\n",
            "secrets": "references_only",
        },
        "meta": {"request_id": "req_t"},
    }


def _prepare_envelope(changes: object, diff: str) -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "plan_id": _PLAN,
            "review_digest": _DIGEST,
            "status": "ready_for_review",
            "expires_at": "2026-09-21T00:15:00Z",
            "changes": changes,
            "no_changes": changes == [],
            "diff_text": diff,
            "operation": "config_apply",
            "deployment_id": _DEPLOYMENT,
        },
        "meta": {"request_id": "req_t"},
    }


# ── export ────────────────────────────────────────────────────────────


@respx.mock
def test_export_writes_document_text(seeded_config: Path, tmp_path: Path) -> None:
    respx.get(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/config").mock(
        return_value=httpx.Response(200, json=_export_envelope()),
    )
    out = tmp_path / "config.json"
    result = runner.invoke(app, ["config", "export", _DEPLOYMENT, "--out", str(out)])
    assert result.exit_code == 0, result.stderr
    assert _DEPLOYMENT in result.stdout
    assert "Exported" in result.stdout
    written = out.read_text(encoding="utf-8")
    assert json.loads(written) == _DOCUMENT


@respx.mock
def test_export_stdout_is_document_only(seeded_config: Path) -> None:
    respx.get(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/config").mock(
        return_value=httpx.Response(200, json=_export_envelope()),
    )
    result = runner.invoke(app, ["config", "export", _DEPLOYMENT])
    assert result.exit_code == 0, result.stderr
    assert json.loads(result.stdout) == _DOCUMENT


# ── apply ─────────────────────────────────────────────────────────────


@respx.mock
def test_apply_flow_carries_exact_digest(seeded_config: Path, tmp_path: Path) -> None:
    doc = tmp_path / "config.json"
    doc.write_text(json.dumps(_DOCUMENT), encoding="utf-8")
    prepare = respx.post(
        f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/prepare-config-apply"
    ).mock(
        return_value=httpx.Response(
            201,
            json=_prepare_envelope(
                ["vars: two changes", "build: one change"],
                "--- a\n+++ b\n@@\n- static_spa: false\n+ static_spa: true\n",
            ),
        )
    )
    apply = respx.post(f"{BASE}/platform/config-plans/{_PLAN}/apply").mock(
        return_value=httpx.Response(
            202,
            json={
                "success": True,
                "data": {
                    "command_id": "cmd_" + "a" * 16,
                    "receipt": {"command_id": "cmd_" + "a" * 16},
                },
                "meta": {"request_id": "req_t"},
            },
        )
    )
    result = runner.invoke(app, ["config", "apply", _DEPLOYMENT, "--file", str(doc), "--yes"])
    assert result.exit_code == 0, result.stderr
    assert "static_spa: true" in result.stdout
    assert "cmd_" + "a" * 16 in result.stdout
    assert "Applied" in result.stdout or "applied" in result.stdout

    prepare_body = json.loads(prepare.calls.last.request.content)
    assert prepare_body == {"document": json.dumps(_DOCUMENT)}
    apply_body = json.loads(apply.calls.last.request.content)
    assert apply_body == {"confirm": True, "review_digest": _DIGEST}


@respx.mock
def test_apply_without_yes_prompts_and_declines(seeded_config: Path, tmp_path: Path) -> None:
    doc = tmp_path / "config.json"
    doc.write_text(json.dumps(_DOCUMENT), encoding="utf-8")
    respx.post(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/prepare-config-apply").mock(
        return_value=httpx.Response(201, json=_prepare_envelope([], ""))
    )
    result = runner.invoke(app, ["config", "apply", _DEPLOYMENT, "--file", str(doc)], input="n\n")
    assert result.exit_code == 0, result.stderr
    assert "Cancelled" in result.stdout


@respx.mock
def test_apply_without_yes_prompts_and_confirms(seeded_config: Path, tmp_path: Path) -> None:
    doc = tmp_path / "config.json"
    doc.write_text(json.dumps(_DOCUMENT), encoding="utf-8")
    respx.post(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/prepare-config-apply").mock(
        return_value=httpx.Response(201, json=_prepare_envelope([], ""))
    )
    respx.post(f"{BASE}/platform/config-plans/{_PLAN}/apply").mock(
        return_value=httpx.Response(
            202,
            json={
                "success": True,
                "data": {"command_id": "cmd_b"},
                "meta": {"request_id": "req_t"},
            },
        )
    )
    result = runner.invoke(app, ["config", "apply", _DEPLOYMENT, "--file", str(doc)], input="y\n")
    assert result.exit_code == 0, result.stderr
    assert "No changes detected" in result.stdout


@respx.mock
def test_apply_digest_mismatch_is_refused(seeded_config: Path, tmp_path: Path) -> None:
    doc = tmp_path / "config.json"
    doc.write_text(json.dumps(_DOCUMENT), encoding="utf-8")
    respx.post(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/prepare-config-apply").mock(
        return_value=httpx.Response(201, json=_prepare_envelope([], ""))
    )
    respx.post(f"{BASE}/platform/config-plans/{_PLAN}/apply").mock(
        return_value=httpx.Response(
            409,
            json={
                "success": False,
                "error": {"code": "CONFLICT", "message": "Config review digest does not match."},
            },
        )
    )
    result = runner.invoke(app, ["config", "apply", _DEPLOYMENT, "--file", str(doc), "--yes"])
    assert result.exit_code != 0
    assert "digest" in result.stdout.lower() or "digest" in result.stderr.lower()


@respx.mock
def test_apply_replayed_plan_renders_receipt(seeded_config: Path, tmp_path: Path) -> None:
    doc = tmp_path / "config.json"
    doc.write_text(json.dumps(_DOCUMENT), encoding="utf-8")
    respx.post(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/prepare-config-apply").mock(
        return_value=httpx.Response(201, json=_prepare_envelope([], ""))
    )
    respx.post(f"{BASE}/platform/config-plans/{_PLAN}/apply").mock(
        return_value=httpx.Response(
            202,
            json={
                "success": True,
                "data": {"replayed": True, "receipt": {"command_id": "cmd_first"}},
                "meta": {"request_id": "req_t"},
            },
        )
    )
    result = runner.invoke(app, ["config", "apply", _DEPLOYMENT, "--file", str(doc), "--yes"])
    assert result.exit_code == 0, result.stderr
    assert "already accepted" in result.stdout


# ── plan ──────────────────────────────────────────────────────────────


@respx.mock
def test_plan_renders_status_and_digest(seeded_config: Path) -> None:
    respx.get(f"{BASE}/platform/config-plans/{_PLAN}").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    "plan_id": _PLAN,
                    "review_digest": _DIGEST,
                    "status": "accepted",
                    "receipt": {"command_id": "cmd_first"},
                },
                "meta": {"request_id": "req_t"},
            },
        )
    )
    result = runner.invoke(app, ["config", "plan", _PLAN])
    assert result.exit_code == 0, result.stderr
    assert "accepted" in result.stdout
    assert _DIGEST in result.stdout
    assert "cmd_first" in result.stdout


@pytest.mark.parametrize(
    "field,value",
    [
        ("deployment_id", "dpl_" + "d" * 16),
        ("operation", "delete"),
        ("plan_id", "cplan_" + "6" * 24 + "/apply"),
        ("review_digest", ""),
        ("changes", None),
        ("changes", {}),
        ("changes", [123]),
        ("diff_text", None),
        ("no_changes", False),
    ],
)
@respx.mock
def test_incomplete_review_never_applies_even_with_yes(
    seeded_config: Path, tmp_path: Path, field, value
) -> None:
    doc = tmp_path / "config.json"
    doc.write_text(json.dumps(_DOCUMENT), encoding="utf-8")
    payload = _prepare_envelope([], "No changes")
    payload["data"][field] = value
    respx.post(f"{BASE}/platform/deployments/custom/{_DEPLOYMENT}/prepare-config-apply").respond(
        201, json=payload
    )
    result = runner.invoke(app, ["config", "apply", _DEPLOYMENT, "--file", str(doc), "--yes"])
    assert result.exit_code == 1
    assert "nothing was applied" in result.stderr
    assert len(respx.calls) == 1


@respx.mock
def test_oversize_file_is_rejected_without_network(seeded_config: Path, tmp_path: Path) -> None:
    doc = tmp_path / "large.json"
    doc.write_bytes(b"x" * 65537)
    result = runner.invoke(app, ["config", "apply", _DEPLOYMENT, "--file", str(doc), "--yes"])
    assert result.exit_code == 1
    assert len(respx.calls) == 0


def test_review_terminal_control_is_visible() -> None:
    from impreza_cli.commands.config import _terminal_text

    assert _terminal_text("before\x1b[2J\r\u202eafter") == "before\\u001b[2J\\u000d\\u202eafter"
