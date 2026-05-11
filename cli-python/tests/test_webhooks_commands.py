"""Unit tests for ``impreza webhook`` (Phase 3.7).

Covers the eight verbs over respx mocks — list / show / create /
update / delete / rotate-secret / deliveries / event-types.
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


@pytest.fixture
def seeded_config(isolated_config: Path) -> Path:
    cfg = Config.load(isolated_config)
    cfg.add_context("personal", api_key=_FAKE_KEY, api_secret=_FAKE_SECRET)
    cfg.save()
    return isolated_config


def _ok(payload: dict[str, object]) -> dict[str, object]:
    return {
        "success": True,
        "data": payload,
        "meta": {"request_id": "req_t"},
    }


def _subscription(
    *,
    id: int = 1,
    url: str = "https://hooks.example.com/impreza",
    events: list[str] | None = None,
    description: str = "",
    is_active: bool = True,
    secret: str | None = None,
    secret_warning: str | None = None,
) -> dict[str, object]:
    return {
        "id": id,
        "url": url,
        "events": events if events is not None else ["topup.paid"],
        "description": description,
        "is_active": is_active,
        "created_at": "2026-05-01T12:00:00Z",
        "secret": secret,
        "secret_warning": secret_warning,
    }


# ── list ────────────────────────────────────────────────────────────


@respx.mock
def test_list_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "webhooks": [
                    _subscription(id=1, url="https://a/h",
                                  events=["topup.paid", "vps.*"]),
                    _subscription(id=2, url="https://b/h",
                                  events=["*"], is_active=False),
                ],
            }),
        )
    )
    result = runner.invoke(app, ["webhook", "list"])
    assert result.exit_code == 0, result.stderr
    # Two rows rendered with ids 1 and 2 (URL column may truncate
    # long values — keep test URLs short).
    assert "https://a/h" in result.stdout or "1" in result.stdout
    assert "https://b/h" in result.stdout or "2" in result.stdout
    # Active / inactive both rendered
    assert "yes" in result.stdout
    assert "no" in result.stdout


@respx.mock
def test_list_empty(seeded_config: Path) -> None:
    respx.get(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(200, json=_ok({"webhooks": []}))
    )
    result = runner.invoke(app, ["webhook", "list"])
    assert result.exit_code == 0
    assert "No webhook subscriptions" in result.stdout


@respx.mock
def test_list_json_emits_events_as_list(seeded_config: Path) -> None:
    """JSON output keeps `events` as a list, not the table-mode
    comma-joined string."""
    respx.get(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "webhooks": [_subscription(id=1, events=["topup.paid", "vps.*"])],
            }),
        )
    )
    result = runner.invoke(app, ["webhook", "list", "--output", "json"])
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed[0]["events"] == ["topup.paid", "vps.*"]


# ── show ────────────────────────────────────────────────────────────


@respx.mock
def test_show(seeded_config: Path) -> None:
    respx.get(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(
            200, json=_ok(_subscription(url="https://hooks/x"))
        )
    )
    result = runner.invoke(app, ["webhook", "show", "1"])
    assert result.exit_code == 0, result.stderr
    assert "https://hooks/x" in result.stdout


# ── create ──────────────────────────────────────────────────────────


@respx.mock
def test_create_returns_secret_once(seeded_config: Path) -> None:
    route = respx.post(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(
            201,
            json=_ok(_subscription(
                id=42, events=["topup.paid", "vps.*"],
                secret="whsec_supersecret_value_here",
            )),
        )
    )
    result = runner.invoke(
        app,
        [
            "webhook", "create",
            "--url", "https://hooks.example.com/impreza",
            "--event", "topup.paid",
            "--event", "vps.*",
            "--description", "production hook",
        ],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {
        "url": "https://hooks.example.com/impreza",
        "events": ["topup.paid", "vps.*"],
        "description": "production hook",
    }
    assert "Subscription 42 created" in result.stdout
    assert "whsec_supersecret_value_here" in result.stdout
    assert "shown only once" in result.stdout


@respx.mock
def test_create_wildcard_event(seeded_config: Path) -> None:
    """The '*' wildcard passes through verbatim."""
    route = respx.post(f"{BASE}/webhooks").mock(
        return_value=httpx.Response(
            201,
            json=_ok(_subscription(events=["*"], secret="whsec_x")),
        )
    )
    result = runner.invoke(
        app,
        [
            "webhook", "create",
            "--url", "https://hooks/x",
            "--event", "*",
        ],
    )
    assert result.exit_code == 0, result.stderr
    body = json.loads(route.calls.last.request.content)
    assert body == {"url": "https://hooks/x", "events": ["*"]}


# ── update ──────────────────────────────────────────────────────────


@respx.mock
def test_update_changes_url_only(seeded_config: Path) -> None:
    """Passing just --url sends a one-field body (PATCH semantics)."""
    route = respx.patch(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_subscription(id=1, url="https://hooks/new")),
        )
    )
    result = runner.invoke(
        app,
        ["webhook", "update", "1", "--url", "https://hooks/new"],
    )
    assert result.exit_code == 0, result.stderr
    body = json.loads(route.calls.last.request.content)
    assert body == {"url": "https://hooks/new"}


@respx.mock
def test_update_activate(seeded_config: Path) -> None:
    route = respx.patch(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_subscription(id=1, is_active=True)),
        )
    )
    result = runner.invoke(
        app, ["webhook", "update", "1", "--activate"]
    )
    assert result.exit_code == 0, result.stderr
    body = json.loads(route.calls.last.request.content)
    assert body == {"is_active": True}


@respx.mock
def test_update_deactivate(seeded_config: Path) -> None:
    route = respx.patch(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_subscription(id=1, is_active=False)),
        )
    )
    result = runner.invoke(
        app, ["webhook", "update", "1", "--deactivate"]
    )
    assert result.exit_code == 0, result.stderr
    body = json.loads(route.calls.last.request.content)
    assert body == {"is_active": False}


def test_update_activate_and_deactivate_mutex(seeded_config: Path) -> None:
    """--activate and --deactivate are mutually exclusive."""
    result = runner.invoke(
        app,
        ["webhook", "update", "1", "--activate", "--deactivate"],
    )
    assert result.exit_code == 1
    assert "mutually exclusive" in result.stderr


def test_update_empty_rejected(seeded_config: Path) -> None:
    """No flags → SDK raises ValueError → CLI exits 1 with the
    SDK message (not a traceback)."""
    with respx.mock:
        result = runner.invoke(app, ["webhook", "update", "1"])
    assert result.exit_code == 1
    assert "at least one" in result.stderr
    assert "Traceback" not in result.stderr


@respx.mock
def test_update_replace_events(seeded_config: Path) -> None:
    """Multiple --event flags replace the events list."""
    route = respx.patch(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_subscription(id=1, events=["topup.paid", "domain.*"])),
        )
    )
    result = runner.invoke(
        app,
        [
            "webhook", "update", "1",
            "--event", "topup.paid",
            "--event", "domain.*",
        ],
    )
    assert result.exit_code == 0, result.stderr
    body = json.loads(route.calls.last.request.content)
    assert body == {"events": ["topup.paid", "domain.*"]}


# ── delete ──────────────────────────────────────────────────────────


@respx.mock
def test_delete_with_yes(seeded_config: Path) -> None:
    route = respx.delete(f"{BASE}/webhooks/1").mock(
        return_value=httpx.Response(200, json=_ok({}))
    )
    result = runner.invoke(app, ["webhook", "delete", "1", "--yes"])
    assert result.exit_code == 0, result.stderr
    assert route.called
    assert "1 deleted" in result.stdout


def test_delete_decline(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app, ["webhook", "delete", "1"], input="n\n"
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


# ── rotate-secret ───────────────────────────────────────────────────


@respx.mock
def test_rotate_secret_prints_new_secret(seeded_config: Path) -> None:
    respx.post(f"{BASE}/webhooks/1/rotate-secret").mock(
        return_value=httpx.Response(
            200,
            json=_ok({"secret": "whsec_rotated_new_value"}),
        )
    )
    result = runner.invoke(
        app, ["webhook", "rotate-secret", "1", "--yes"]
    )
    assert result.exit_code == 0, result.stderr
    assert "whsec_rotated_new_value" in result.stdout
    assert "shown only once" in result.stdout


def test_rotate_secret_decline(seeded_config: Path) -> None:
    with respx.mock:
        result = runner.invoke(
            app, ["webhook", "rotate-secret", "1"], input="n\n"
        )
    assert result.exit_code == 0
    assert "Cancelled" in result.stdout


@respx.mock
def test_rotate_secret_empty_response_exits_1(seeded_config: Path) -> None:
    """If the server omits the secret on rotate, treat as error."""
    respx.post(f"{BASE}/webhooks/1/rotate-secret").mock(
        return_value=httpx.Response(200, json=_ok({}))
    )
    result = runner.invoke(
        app, ["webhook", "rotate-secret", "1", "--yes"]
    )
    assert result.exit_code == 1
    assert "no secret was returned" in result.stderr


# ── deliveries ──────────────────────────────────────────────────────


@respx.mock
def test_deliveries_renders_table(seeded_config: Path) -> None:
    respx.get(f"{BASE}/webhooks/1/deliveries").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "deliveries": [
                    {"id": 100, "event_type": "topup.paid",
                     "event_id": "evt_abc", "attempts": 1,
                     "last_attempted_at": "2026-05-09T10:00:00Z",
                     "last_response_code": 200,
                     "delivered": True,
                     "delivered_at": "2026-05-09T10:00:01Z"},
                    {"id": 101, "event_type": "vps.power_state_changed",
                     "event_id": "evt_xyz", "attempts": 3,
                     "last_attempted_at": "2026-05-10T11:00:00Z",
                     "last_response_code": 500,
                     "last_error": "connection refused",
                     "delivered": False},
                ],
            }),
        )
    )
    result = runner.invoke(app, ["webhook", "deliveries", "1"])
    assert result.exit_code == 0, result.stderr
    # Table truncates long values; assert the response codes which
    # render fully + the success/failure mix via the delivered column.
    assert "200" in result.stdout
    assert "500" in result.stdout
    assert "yes" in result.stdout  # delivered=true row
    assert "no" in result.stdout   # delivered=false row


@respx.mock
def test_deliveries_empty(seeded_config: Path) -> None:
    respx.get(f"{BASE}/webhooks/1/deliveries").mock(
        return_value=httpx.Response(
            200, json=_ok({"deliveries": []})
        )
    )
    result = runner.invoke(app, ["webhook", "deliveries", "1"])
    assert result.exit_code == 0
    assert "No delivery history" in result.stdout


# ── event-types ─────────────────────────────────────────────────────


@respx.mock
def test_event_types_table_mode(seeded_config: Path) -> None:
    respx.get(f"{BASE}/webhooks/event-types").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "event_types": [
                    "topup.paid",
                    "vps.power_state_changed",
                    "domain.registered",
                ],
                "wildcards": {
                    "vps.*": "Every event whose type starts with vps.",
                    "*": "Every event the server emits.",
                },
            }),
        )
    )
    result = runner.invoke(app, ["webhook", "event-types"])
    assert result.exit_code == 0, result.stderr
    assert "topup.paid" in result.stdout
    assert "vps.power_state_changed" in result.stdout
    assert "vps.*" in result.stdout
    assert "Every event whose type starts with vps." in result.stdout


@respx.mock
def test_event_types_json_mode(seeded_config: Path) -> None:
    respx.get(f"{BASE}/webhooks/event-types").mock(
        return_value=httpx.Response(
            200,
            json=_ok({
                "event_types": ["topup.paid"],
                "wildcards": {"*": "all"},
            }),
        )
    )
    result = runner.invoke(
        app, ["webhook", "event-types", "--output", "json"]
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["event_types"] == ["topup.paid"]
    assert parsed["wildcards"] == {"*": "all"}
