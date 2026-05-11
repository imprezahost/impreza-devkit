"""Unit tests for ``impreza account topup`` and ``topup-status``
(Phase 3.6). Covers the two new account verbs over the SDK's
TopupInvoice future (1.7).
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


def _topup_payload(
    *,
    invoice_id: int = 500,
    amount: float = 50.0,
    currency: str = "USD",
    method: str | None = "xmr",
    status: str = "pending",
    payment_url: str | None = "https://btcpay.example/inv/500",
    expires_at: str | None = "2026-05-11T20:00:00Z",
    paid_at: str | None = None,
    balance_after: float | None = None,
) -> dict[str, object]:
    return {
        "invoice_id": invoice_id,
        "amount": amount,
        "currency": currency,
        "method": method,
        "status": status,
        "payment_url": payment_url,
        "expires_at": expires_at,
        "paid_at": paid_at,
        "balance_after": balance_after,
    }


def _patch_sleep() -> None:
    """Patch the account module's time.sleep so --wait doesn't actually
    block on the 30-second poll interval."""
    import impreza_cli.commands.account as account_mod

    account_mod.time.sleep = lambda _: None  # type: ignore[assignment, return-value]


# ══════════════════════════════════════════════════════════════════════
# topup
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_topup_creates_invoice_without_wait(seeded_config: Path) -> None:
    """Default flow: POST returns the invoice with payment_url; CLI
    renders the details and exits."""
    route = respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )
    result = runner.invoke(
        app,
        ["account", "topup", "--amount", "50", "--method", "xmr"],
    )
    assert result.exit_code == 0, result.stderr
    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"amount": 50.0, "method": "xmr"}
    # Payment URL is the critical field — must surface to the user
    assert "https://btcpay.example/inv/500" in result.stdout
    assert "500" in result.stdout
    assert "pending" in result.stdout


@respx.mock
def test_topup_json_output(seeded_config: Path) -> None:
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )
    result = runner.invoke(
        app,
        ["account", "topup", "--amount", "50", "--output", "json"],
    )
    assert result.exit_code == 0, result.stderr
    parsed = json.loads(result.stdout)
    assert parsed["invoice_id"] == 500
    assert parsed["amount"] == 50.0
    assert parsed["status"] == "pending"
    assert parsed["payment_url"] == "https://btcpay.example/inv/500"


@respx.mock
def test_topup_with_wait_polls_to_paid(seeded_config: Path) -> None:
    """--wait polls the status endpoint until terminal-paid."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )
    respx.get(f"{BASE}/account/topup/500").mock(
        side_effect=[
            httpx.Response(
                200,
                json=_ok(_topup_payload(status="pending", payment_url=None,
                                        expires_at=None)),
            ),
            httpx.Response(
                200,
                json=_ok(_topup_payload(
                    status="paid", payment_url=None, expires_at=None,
                    paid_at="2026-05-11T19:00:00Z",
                    balance_after=100.0,
                )),
            ),
        ]
    )

    _patch_sleep()
    result = runner.invoke(
        app,
        ["account", "topup", "--amount", "50", "--wait"],
    )
    assert result.exit_code == 0, result.stderr
    # Both renders (just-created and settled) should appear
    assert "(just created)" in result.stdout
    assert "(settled)" in result.stdout
    # Progress line redraws elapsed/ETA via \r — accumulated buffer
    # contains the line text at least once.
    assert "elapsed" in result.stdout
    # Final settled line + status
    assert "settled: status='paid'" in result.stdout


@respx.mock
def test_topup_with_wait_failed_state_exits_1(seeded_config: Path) -> None:
    """Terminal failure (cancelled / refunded / expired) → exit 1
    with a clear stderr message."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )
    respx.get(f"{BASE}/account/topup/500").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_topup_payload(
                status="cancelled", payment_url=None, expires_at=None,
            )),
        )
    )

    _patch_sleep()
    result = runner.invoke(
        app,
        ["account", "topup", "--amount", "50", "--wait"],
    )
    assert result.exit_code == 1
    assert "500" in result.stderr
    assert "cancelled" in result.stderr


@respx.mock
def test_topup_already_paid_skips_polling(seeded_config: Path) -> None:
    """Edge case: the create response itself is already terminal
    (paid). CLI shouldn't poll — should just report and exit."""
    poll_route = respx.get(f"{BASE}/account/topup/500").mock(
        return_value=httpx.Response(500, text="should-not-be-called")
    )
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(
            201,
            json=_ok(_topup_payload(
                status="paid", paid_at="2026-05-11T19:00:00Z",
                balance_after=100.0,
            )),
        )
    )
    result = runner.invoke(
        app,
        ["account", "topup", "--amount", "50", "--wait"],
    )
    assert result.exit_code == 0, result.stderr
    assert not poll_route.called
    assert "already 'paid'" in result.stdout


def test_topup_negative_amount_rejected_by_typer(seeded_config: Path) -> None:
    """Typer's `min=0` rejects negative amounts before any HTTP."""
    result = runner.invoke(
        app, ["account", "topup", "--amount", "-10"],
    )
    assert result.exit_code != 0


# ══════════════════════════════════════════════════════════════════════
# 4.2 polish: --browser flag
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_topup_browser_opens_payment_url(seeded_config: Path) -> None:
    """--browser calls webbrowser.open() with the payment_url. The
    test patches the stdlib webbrowser.open to capture the URL
    rather than actually spawning a browser."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )
    captured: list[str] = []

    import impreza_cli.commands.account as account_mod
    original = account_mod.webbrowser.open
    account_mod.webbrowser.open = lambda url: captured.append(url) or True  # type: ignore[assignment]
    try:
        result = runner.invoke(
            app,
            ["account", "topup", "--amount", "50", "--browser"],
        )
    finally:
        account_mod.webbrowser.open = original  # type: ignore[assignment]

    assert result.exit_code == 0, result.stderr
    assert captured == ["https://btcpay.example/inv/500"]
    assert "(payment URL opened in your default browser)" in result.stdout


@respx.mock
def test_topup_browser_unavailable_prints_fallback(seeded_config: Path) -> None:
    """When webbrowser.open() returns False (no GUI / headless),
    the CLI prints a fallback message but still exits cleanly."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )

    import impreza_cli.commands.account as account_mod
    original = account_mod.webbrowser.open
    account_mod.webbrowser.open = lambda url: False  # type: ignore[assignment]
    try:
        result = runner.invoke(
            app,
            ["account", "topup", "--amount", "50", "--browser"],
        )
    finally:
        account_mod.webbrowser.open = original  # type: ignore[assignment]

    assert result.exit_code == 0, result.stderr
    assert "no default browser configured" in result.stdout


@respx.mock
def test_topup_browser_raises_prints_friendly_error(
    seeded_config: Path,
) -> None:
    """Some headless Linux setups raise from webbrowser.open() rather
    than returning False. CLI should catch and continue, not crash."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )

    def _raise(url: str) -> bool:
        raise RuntimeError("no DISPLAY")

    import impreza_cli.commands.account as account_mod
    original = account_mod.webbrowser.open
    account_mod.webbrowser.open = _raise  # type: ignore[assignment]
    try:
        result = runner.invoke(
            app,
            ["account", "topup", "--amount", "50", "--browser"],
        )
    finally:
        account_mod.webbrowser.open = original  # type: ignore[assignment]

    assert result.exit_code == 0, result.stderr
    assert "could not open browser" in result.stdout
    assert "no DISPLAY" in result.stdout


@respx.mock
def test_topup_browser_suppressed_in_json_mode(seeded_config: Path) -> None:
    """JSON consumers typically handle the payment URL themselves —
    silently suppress --browser so it doesn't surprise scripts."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )
    captured: list[str] = []

    import impreza_cli.commands.account as account_mod
    original = account_mod.webbrowser.open
    account_mod.webbrowser.open = lambda url: captured.append(url) or True  # type: ignore[assignment]
    try:
        result = runner.invoke(
            app,
            [
                "account", "topup",
                "--amount", "50",
                "--browser",
                "--output", "json",
            ],
        )
    finally:
        account_mod.webbrowser.open = original  # type: ignore[assignment]

    assert result.exit_code == 0
    assert captured == []        # browser.open never called
    parsed = json.loads(result.stdout)
    assert parsed["invoice_id"] == 500


@respx.mock
def test_topup_browser_no_payment_url(seeded_config: Path) -> None:
    """If the server somehow returns an invoice with no payment_url
    (already-settled edge case), --browser prints a clear note."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(
            201,
            json=_ok(_topup_payload(payment_url=None, status="paid")),
        )
    )

    import impreza_cli.commands.account as account_mod
    original = account_mod.webbrowser.open
    account_mod.webbrowser.open = lambda url: True  # type: ignore[assignment]
    try:
        result = runner.invoke(
            app,
            ["account", "topup", "--amount", "50", "--browser"],
        )
    finally:
        account_mod.webbrowser.open = original  # type: ignore[assignment]

    assert result.exit_code == 0, result.stderr
    assert "no payment_url returned" in result.stdout


# ══════════════════════════════════════════════════════════════════════
# 4.2 polish: poll ETA rendering
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_topup_wait_progress_includes_expiry_eta(seeded_config: Path) -> None:
    """The redrawn progress line shows 'Xs elapsed / Ys until expiry'
    when the invoice's expires_at parses to a future timestamp.

    Picks an expires_at far in the future relative to the test
    machine clock so the math is stable regardless of when the
    test runs."""
    from datetime import datetime, timedelta
    from datetime import timezone as _tz
    future = (datetime.now(_tz.utc) + timedelta(hours=2)).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(
            201, json=_ok(_topup_payload(expires_at=future))
        )
    )
    respx.get(f"{BASE}/account/topup/500").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_topup_payload(status="paid", payment_url=None,
                                    expires_at=None,
                                    paid_at="2026-05-11T19:00:00Z",
                                    balance_after=100.0)),
        )
    )
    _patch_sleep()
    result = runner.invoke(
        app, ["account", "topup", "--amount", "50", "--wait"]
    )
    assert result.exit_code == 0, result.stderr
    assert "elapsed" in result.stdout
    # Future expiry → 'until expiry' portion appears
    assert "until expiry" in result.stdout


@respx.mock
def test_topup_wait_progress_omits_expiry_when_missing(
    seeded_config: Path,
) -> None:
    """When expires_at is None, the progress line just shows
    elapsed — no 'until expiry' portion."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(
            201,
            json=_ok(_topup_payload(expires_at=None)),
        )
    )
    respx.get(f"{BASE}/account/topup/500").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_topup_payload(
                status="paid", payment_url=None, expires_at=None,
                paid_at="2026-05-11T19:00:00Z", balance_after=100.0,
            )),
        )
    )
    _patch_sleep()
    result = runner.invoke(
        app, ["account", "topup", "--amount", "50", "--wait"]
    )
    assert result.exit_code == 0, result.stderr
    assert "elapsed" in result.stdout
    # No expiry → 'until expiry' string must NOT appear
    assert "until expiry" not in result.stdout


@respx.mock
def test_topup_wait_timeout_preserves_payment_url(seeded_config: Path) -> None:
    """Timeout error message must include the payment_url so the
    user can retry payment manually."""
    respx.post(f"{BASE}/account/topup").mock(
        return_value=httpx.Response(201, json=_ok(_topup_payload()))
    )
    # Status endpoint always says pending — eventually hits --timeout.
    respx.get(f"{BASE}/account/topup/500").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_topup_payload(payment_url=None, expires_at=None)),
        )
    )
    _patch_sleep()
    result = runner.invoke(
        app,
        ["account", "topup", "--amount", "50", "--wait", "--timeout", "60"],
    )
    assert result.exit_code == 1
    assert "not paid within 60s" in result.stderr
    # The original payment URL must be carried through to the error
    # message so the user can copy it
    assert "https://btcpay.example/inv/500" in result.stderr


def test_seconds_until_expiry_helper() -> None:
    """Direct test of the ISO-parsing helper for the cases the
    poll renderer depends on."""
    from impreza_cli.commands.account import _seconds_until_expiry

    # None / empty
    assert _seconds_until_expiry(None) is None
    assert _seconds_until_expiry("") is None
    # Unparseable
    assert _seconds_until_expiry("not a date") is None
    # Future timestamp with Z suffix
    from datetime import datetime, timedelta
    from datetime import timezone as _tz
    future = (datetime.now(_tz.utc) + timedelta(seconds=120)).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    sec = _seconds_until_expiry(future)
    assert sec is not None and 100 < sec < 140
    # Past timestamp returns a negative number
    past = (datetime.now(_tz.utc) - timedelta(seconds=120)).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    sec = _seconds_until_expiry(past)
    assert sec is not None and sec < 0


# ══════════════════════════════════════════════════════════════════════
# topup-status
# ══════════════════════════════════════════════════════════════════════


@respx.mock
def test_topup_status_renders_pending(seeded_config: Path) -> None:
    """Status endpoint doesn't echo payment_url / expires_at — that's
    expected behaviour per the SDK docs."""
    respx.get(f"{BASE}/account/topup/500").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_topup_payload(
                status="pending", payment_url=None, expires_at=None,
            )),
        )
    )
    result = runner.invoke(app, ["account", "topup-status", "500"])
    assert result.exit_code == 0, result.stderr
    assert "500" in result.stdout
    assert "pending" in result.stdout


@respx.mock
def test_topup_status_renders_paid_with_balance(seeded_config: Path) -> None:
    respx.get(f"{BASE}/account/topup/500").mock(
        return_value=httpx.Response(
            200,
            json=_ok(_topup_payload(
                status="paid", payment_url=None, expires_at=None,
                paid_at="2026-05-11T19:00:00Z", balance_after=100.0,
            )),
        )
    )
    result = runner.invoke(
        app, ["account", "topup-status", "500", "--output", "json"]
    )
    assert result.exit_code == 0
    parsed = json.loads(result.stdout)
    assert parsed["status"] == "paid"
    assert parsed["balance_after"] == 100.0


@respx.mock
def test_topup_status_404(seeded_config: Path) -> None:
    """Unknown invoice id surfaces the standard ApiError stderr line."""
    respx.get(f"{BASE}/account/topup/9999").mock(
        return_value=httpx.Response(
            404,
            json={
                "success": False,
                "meta": {"request_id": "req_t"},
                "error": {"code": "NOT_FOUND", "message": "Invoice not found."},
            },
        )
    )
    result = runner.invoke(app, ["account", "topup-status", "9999"])
    assert result.exit_code == 1
    assert "Invoice not found" in result.stderr
    assert "Traceback" not in result.stderr
