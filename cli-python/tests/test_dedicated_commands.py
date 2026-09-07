"""Unit tests for the ``impreza dedicated`` surface.

This module exists because the whole 20-verb surface once shipped dead:
every command passed the raw :class:`typer.Context` to
``make_client_or_exit`` (which wants a ``GlobalState``) and called
``resolve_output`` / ``print_table`` / ``print_dict`` with signatures
that never existed. Nothing caught it, because ``dedicated`` was the one
command module with no test file.

So the point of these tests is coverage of the *plumbing*, not of the
individual payload shapes: invoke each shape of verb through the real
Typer app, so a context/state or output-helper regression fails here
instead of in a user's terminal.
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


def _ok(payload: object = None) -> dict[str, object]:
    return {
        "success": True,
        "data": payload if payload is not None else {},
        "meta": {"request_id": "req_t"},
    }


_SERVERS = [
    {
        "service_id": 101,
        "domain": "srv1.example.com",
        "ip": "203.0.113.10",
        "status": "Active",
        "capabilities": ["firewall", "kvm"],
    },
    {
        "service_id": 102,
        "domain": "srv2.example.com",
        "ip": "203.0.113.11",
        "status": "Active",
        "capabilities": ["kvm"],
    },
]


# ── discovery: list ────────────────────────────────────────────────────


@respx.mock
def test_list_renders_table(seeded_config: Path) -> None:
    """``dedicated list`` reaches the API and renders the row table.

    Regression guard: this used to raise ``AttributeError`` on the
    GlobalState lookup before the request was ever made.
    """
    route = respx.get(f"{BASE}/dedicated").mock(
        return_value=httpx.Response(200, json=_ok(_SERVERS))
    )
    result = runner.invoke(app, ["dedicated", "list"])
    assert result.exit_code == 0, result.stdout
    assert route.called
    assert "srv1.example.com" in result.stdout
    assert "203.0.113.11" in result.stdout


@respx.mock
def test_list_json_echoes_raw_payload(seeded_config: Path) -> None:
    """JSON output passes the server payload through untouched, so
    scripts see every field — not just the table projection."""
    respx.get(f"{BASE}/dedicated").mock(
        return_value=httpx.Response(200, json=_ok(_SERVERS))
    )
    result = runner.invoke(app, ["--output", "json", "dedicated", "list"])
    assert result.exit_code == 0, result.stdout
    assert json.loads(result.stdout) == _SERVERS


@respx.mock
def test_list_empty_is_not_an_error(seeded_config: Path) -> None:
    respx.get(f"{BASE}/dedicated").mock(return_value=httpx.Response(200, json=_ok([])))
    result = runner.invoke(app, ["dedicated", "list"])
    assert result.exit_code == 0, result.stdout
    assert "No dedicated servers" in result.stdout


# ── discovery: single-resource dict verbs ──────────────────────────────


@pytest.mark.parametrize(
    ("argv", "path", "payload", "needle"),
    [
        (
            ["dedicated", "show", "101"],
            "/dedicated/101",
            {"service_id": 101, "domain": "srv1.example.com"},
            "srv1.example.com",
        ),
        (
            ["dedicated", "capabilities", "101"],
            "/dedicated/101/capabilities",
            {"capabilities": ["firewall", "kvm"]},
            "firewall",
        ),
        (
            ["dedicated", "status", "101"],
            "/dedicated/101/status",
            {"power": "on"},
            "on",
        ),
        (
            ["dedicated", "ips", "101"],
            "/dedicated/101/ips",
            {"ips": [{"ip": "203.0.113.10"}]},
            "203.0.113.10",
        ),
        (
            ["dedicated", "kvm", "101"],
            "/dedicated/101/kvm",
            {"url": "https://kvm.example.com/session"},
            "kvm.example.com",
        ),
        (
            ["dedicated", "firewall", "101"],
            "/dedicated/101/firewall",
            {"state": "always_on"},
            "always_on",
        ),
        (
            ["dedicated", "vpn", "101"],
            "/dedicated/101/vpn",
            {"username": "vpnuser"},
            "vpnuser",
        ),
    ],
)
@respx.mock
def test_dict_verbs_render(
    seeded_config: Path,
    argv: list[str],
    path: str,
    payload: dict[str, object],
    needle: str,
) -> None:
    """Every read verb that returns a dict renders a Field / Value table."""
    route = respx.get(f"{BASE}{path}").mock(
        return_value=httpx.Response(200, json=_ok(payload))
    )
    result = runner.invoke(app, argv)
    assert result.exit_code == 0, result.stdout
    assert route.called
    assert needle in result.stdout


@respx.mock
def test_os_images_renders_list(seeded_config: Path) -> None:
    """``os-images`` returns a list[dict] — the other ``_emit`` branch."""
    images = [
        {"os_id": "ubuntu-24.04", "label": "Ubuntu 24.04 LTS"},
        {"os_id": "debian-12", "label": "Debian 12"},
    ]
    respx.get(f"{BASE}/dedicated/101/os-images").mock(
        return_value=httpx.Response(200, json=_ok(images))
    )
    result = runner.invoke(app, ["dedicated", "os-images", "101"])
    assert result.exit_code == 0, result.stdout
    assert "ubuntu-24.04" in result.stdout
    assert "Debian 12" in result.stdout


# ── rDNS ───────────────────────────────────────────────────────────────


@respx.mock
def test_set_rdns_puts_ip_in_path_and_hostname_in_body(seeded_config: Path) -> None:
    """The IP is a path segment, not a body field — a swap here would
    silently retarget the PTR write at the wrong address."""
    route = respx.put(f"{BASE}/dedicated/101/ips/203.0.113.10/rdns").mock(
        return_value=httpx.Response(200, json=_ok({"status": "queued"}))
    )
    result = runner.invoke(
        app,
        [
            "dedicated",
            "set-rdns",
            "101",
            "--ip",
            "203.0.113.10",
            "--hostname",
            "srv1.example.com",
        ],
    )
    assert result.exit_code == 0, result.stdout
    assert route.called
    assert json.loads(route.calls.last.request.content) == {
        "hostname": "srv1.example.com"
    }


@respx.mock
def test_reset_rdns(seeded_config: Path) -> None:
    route = respx.post(f"{BASE}/dedicated/101/ips/rdns/reset").mock(
        return_value=httpx.Response(200, json=_ok({"status": "queued"}))
    )
    result = runner.invoke(app, ["dedicated", "reset-rdns", "101"])
    assert result.exit_code == 0, result.stdout
    assert route.called


# ── power ──────────────────────────────────────────────────────────────


@pytest.mark.parametrize("verb", ["start", "shutdown", "reboot"])
@respx.mock
def test_power_verbs(seeded_config: Path, verb: str) -> None:
    route = respx.post(f"{BASE}/dedicated/101/{verb}").mock(
        return_value=httpx.Response(200, json=_ok())
    )
    result = runner.invoke(app, ["dedicated", verb, "101"])
    assert result.exit_code == 0, result.stdout
    assert route.called


# ── reinstall (destructive) ────────────────────────────────────────────


def test_reinstall_requires_confirm_flag(seeded_config: Path) -> None:
    """Without ``--confirm`` the CLI refuses before any HTTP call, so a
    mistyped command can never wipe a server."""
    result = runner.invoke(
        app,
        [
            "dedicated",
            "reinstall",
            "101",
            "--os-id",
            "ubuntu-24.04",
            "--password",
            "placeholder-not-a-real-password",
            "--yes",
        ],
    )
    assert result.exit_code == 1
    assert "--confirm" in result.stderr


@respx.mock
def test_reinstall_sends_wipe_header(seeded_config: Path) -> None:
    """The API demands ``X-Impreza-Confirm: WIPE`` alongside the body
    flag; the SDK injects it and the CLI must not strip it."""
    route = respx.post(f"{BASE}/dedicated/101/reinstall").mock(
        return_value=httpx.Response(200, json=_ok({"status": "queued"}))
    )
    result = runner.invoke(
        app,
        [
            "dedicated",
            "reinstall",
            "101",
            "--os-id",
            "ubuntu-24.04",
            "--password",
            "placeholder-not-a-real-password",
            "--confirm",
            "--yes",
        ],
    )
    assert result.exit_code == 0, result.stdout
    assert route.called
    assert route.calls.last.request.headers["X-Impreza-Confirm"] == "WIPE"
    body = json.loads(route.calls.last.request.content)
    assert body["confirm"] is True
    assert body["os_id"] == "ubuntu-24.04"
