"""``impreza doctor`` health-check command — Phase 4.1.

A single-command first-line support tool. Runs a sequence of
checks against the resolved context and reports the state of
each, so a user troubleshooting "I just installed and nothing
works" gets a copy-pasteable diagnostic instead of digging
through eight different verbs.

Checks (in order, stopping early on hard failures):

1. **Config file resolved** — does ``~/.config/impreza/config.toml``
   exist and parse? (Implicit via ``make_client_or_exit``; failures
   land as the standard config-error stderr.)
2. **Active context** — which context resolved? Show its label
   and the API key prefix.
3. **API reachable** — round-trip ``GET /account/api-keys/self``.
   Network errors and auth errors both surface here.
4. **Key status** — the returned ``KeyIdentity.status`` field.
5. **IP whitelist** — does the server-observed ``request_ip`` match
   one of the whitelist entries? Mismatch is the single most
   common "everything is 403" cause.
6. **Account profile + balance** — round-trip ``GET /account``.
   Sanity check that the key has actual scopes beyond
   ``/account/api-keys/self``.

Each check renders as ``[OK] / [FAIL] / [WARN]`` (ASCII-only —
no Unicode glyphs, per the Phase 1.6 cp1252 lesson). The exit
code is 0 if every check passed, 1 otherwise. Output mode
``--output json`` emits a structured array suitable for piping
into monitoring scripts.
"""

from __future__ import annotations

import json
import sys
import time
from typing import Any

import typer
from impreza.exceptions import (
    ApiError,
    AuthError,
    IpNotWhitelisted,
    NetworkError,
    PermissionDenied,
)

from ..output import OutputFormat, error, success, warning
from ..sdk import make_client_or_exit
from ..state import from_typer_context, resolve_output

app = typer.Typer(
    name="doctor",
    help="Run a health check against the active context.",
    invoke_without_command=True,
    no_args_is_help=False,
)


# ── data model ──────────────────────────────────────────────────────


class _CheckResult:
    """One row in the doctor report. Mutable so callers can build it
    in-place across the check body.

    ``ok`` is True/False/None — None means "skipped" (typically
    because an earlier check failed and this one depends on it).
    """

    __slots__ = ("name", "ok", "summary", "detail")

    def __init__(self, name: str) -> None:
        self.name: str = name
        self.ok: bool | None = False
        self.summary: str = ""
        self.detail: str = ""

    def passed(self, summary: str, detail: str = "") -> None:
        self.ok = True
        self.summary = summary
        self.detail = detail

    def failed(self, summary: str, detail: str = "") -> None:
        self.ok = False
        self.summary = summary
        self.detail = detail

    def warned(self, summary: str, detail: str = "") -> None:
        # WARN renders distinctly but still counts as "passed" for
        # the overall exit code. Used for cosmetic mismatches that
        # don't actually break functionality.
        self.ok = True
        self.summary = "WARN: " + summary
        self.detail = detail

    def skipped(self, summary: str) -> None:
        self.ok = None
        self.summary = summary
        self.detail = ""

    def as_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "ok": self.ok,
            "summary": self.summary,
            "detail": self.detail,
        }


# ── individual checks ───────────────────────────────────────────────


def _check_context(state: Any) -> _CheckResult:
    """Report which context resolved. Doesn't actually verify the
    API key — that's the next check's job. The ``make_client_or_exit``
    call in the parent already failed-fast on missing-config errors,
    so reaching this check at all means there's a usable context."""
    r = _CheckResult("active-context")
    override = state.context_override
    if override:
        r.passed(f"Context override: {override!r}")
    else:
        r.passed("Default context")
    return r


def _check_api_reachable(client: Any, key_holder: dict[str, Any]) -> _CheckResult:
    """Round-trip GET /account/api-keys/self. Captures elapsed time
    so the report shows latency, and stashes the result in
    ``key_holder`` so later checks reuse it without a second HTTP."""
    r = _CheckResult("api-reachable")
    t0 = time.monotonic()
    try:
        key = client.account.api_key_self()
    except NetworkError as exc:
        r.failed(
            "Could not reach api.imprezahost.com",
            f"Network error: {exc}. Check connectivity / DNS / proxy. "
            "If using Tor, confirm the SOCKS proxy is up.",
        )
        return r
    except AuthError as exc:
        r.failed(
            "Authentication failed (HTTP 401)",
            f"{exc.message}. The API key or secret is invalid. "
            "Rotate via Impreza Account, update the local context "
            "with `impreza context create / use`.",
        )
        return r
    except IpNotWhitelisted as exc:
        r.failed(
            "IP not whitelisted (HTTP 403)",
            f"{exc.message}. Add the calling IP to the API key's "
            "whitelist via Impreza Account, or use a different "
            "key whose whitelist already covers this IP.",
        )
        return r
    except PermissionDenied as exc:
        r.failed(
            "Permission denied (HTTP 403)",
            f"{exc.message}.",
        )
        return r
    except ApiError as exc:
        r.failed(
            f"API error: {exc.message}",
            f"code={exc.code or '?'}, status={exc.status_code or '?'}",
        )
        return r
    elapsed_ms = (time.monotonic() - t0) * 1000.0
    key_holder["key"] = key
    r.passed(
        f"GET /account/api-keys/self OK ({elapsed_ms:.0f}ms)",
        f"key prefix={key.prefix!r}, label={key.label or '(unnamed)'!r}",
    )
    return r


def _check_key_status(key_holder: dict[str, Any]) -> _CheckResult:
    """Inspect the KeyIdentity.status field returned by api_key_self.
    Active keys pass; anything else (paused, revoked, etc.) flags as
    a hard fail — even though the call succeeded, the key may stop
    working at any time."""
    r = _CheckResult("key-status")
    key = key_holder.get("key")
    if key is None:
        r.skipped("api-reachable check failed; cannot inspect key")
        return r
    status = (key.status or "").lower()
    if status == "active":
        r.passed(f"status={status!r}")
    else:
        r.failed(
            f"status={key.status!r} (not active)",
            "Pending, paused, or revoked keys will start returning "
            "401 unpredictably. Rotate via Impreza Account.",
        )
    return r


def _check_ip_whitelist(key_holder: dict[str, Any]) -> _CheckResult:
    """Compare ``request_ip`` (what the server saw the call coming
    from) to ``ip_whitelist`` (the entries the server would accept)."""
    r = _CheckResult("ip-whitelist")
    key = key_holder.get("key")
    if key is None:
        r.skipped("api-reachable check failed; cannot inspect whitelist")
        return r

    request_ip = key.request_ip or ""
    entries = list(key.ip_whitelist or [])
    if not request_ip:
        r.warned(
            "server did not echo request_ip in this response",
            "Whitelist check is unavailable; the call already "
            "succeeded so the IP must be allowed, but the report "
            "cannot prove it.",
        )
        return r
    if not entries:
        r.warned(
            f"request_ip {request_ip} reached the API but the key "
            "has no whitelist entries",
            "Either the key has whitelist enforcement disabled "
            "(unusual) or the server is letting it through anyway. "
            "Inspect via Impreza Account to be sure.",
        )
        return r

    match = next((e for e in entries if e.ip_address == request_ip), None)
    if match is None:
        labels = ", ".join(
            f"{e.ip_address!r}{f' ({e.label!r})' if e.label else ''}"
            for e in entries
        )
        r.failed(
            f"request_ip {request_ip} not in whitelist "
            f"({len(entries)} entr{'y' if len(entries) == 1 else 'ies'})",
            f"Whitelist: {labels}. Add the calling IP via your Impreza "
            "Account, or switch to a context whose key already "
            "allows this IP.",
        )
        return r

    label = f" ({match.label!r})" if match.label else ""
    r.passed(f"request_ip {request_ip} matches entry{label}")
    return r


def _check_account_profile(client: Any) -> _CheckResult:
    """Round-trip GET /account. The api_key_self endpoint sometimes
    bypasses scope checks; calling /account exercises a regular
    read-scope so the doctor catches scope-limited keys early."""
    r = _CheckResult("account-profile")
    try:
        acc = client.account.get()
    except ApiError as exc:
        r.failed(
            f"GET /account failed: {exc.message}",
            f"code={exc.code or '?'}, status={exc.status_code or '?'}. "
            "If api-reachable passed but this didn't, the key may "
            "lack the basic read scope. Contact support.",
        )
        return r
    name = f"{acc.first_name} {acc.last_name}".strip()
    if acc.company:
        name = f"{name} ({acc.company})"
    r.passed(
        f"{name} <{acc.email}>, balance {acc.balance:.2f} {acc.currency}",
        f"registered {acc.registered_at}",
    )
    return r


# ── renderers ───────────────────────────────────────────────────────


_LABEL_COLOR = {
    True: typer.colors.GREEN,
    False: typer.colors.RED,
    None: typer.colors.YELLOW,
}
_LABEL_TEXT = {True: "[OK]  ", False: "[FAIL]", None: "[SKIP]"}


def _is_warn(result: _CheckResult) -> bool:
    return result.ok is True and result.summary.startswith("WARN:")


def _print_text_report(results: list[_CheckResult]) -> None:
    """Write the human-friendly report to stdout. Failures and warns
    get their detail indented under the summary line so a copy-paste
    of the whole report preserves the full context."""
    sys.stdout.write("\n")
    sys.stdout.write("impreza doctor\n")
    sys.stdout.write("-" * 40 + "\n")
    for r in results:
        if _is_warn(r):
            label = "[WARN]"
            color = typer.colors.YELLOW
        else:
            label = _LABEL_TEXT[r.ok]
            color = _LABEL_COLOR[r.ok]
        typer.secho(label, fg=color, bold=True, nl=False)
        sys.stdout.write(f"  {r.name}: {r.summary}\n")
        if r.detail:
            for line in r.detail.splitlines():
                sys.stdout.write(f"        {line}\n")
    sys.stdout.write("-" * 40 + "\n")


def _print_summary(results: list[_CheckResult]) -> int:
    """Bottom-line summary. Returns the exit code (0 if all pass).

    Uses the :mod:`commands.output` palette helpers (4.3) so the
    summary line matches the colour conventions every other CLI
    command will adopt going forward: success = green, warning =
    yellow, error = red.
    """
    total = len(results)
    failed = [r for r in results if r.ok is False]
    skipped = [r for r in results if r.ok is None]
    warned = [r for r in results if _is_warn(r)]
    passed = total - len(failed) - len(skipped)
    if failed:
        error(f"{len(failed)} of {total} checks failed.")
        return 1
    if warned or skipped:
        msg_parts = [f"{passed} of {total} checks passed"]
        if warned:
            msg_parts.append(f"{len(warned)} warning(s)")
        if skipped:
            msg_parts.append(f"{len(skipped)} skipped")
        warning(", ".join(msg_parts) + ".")
        return 0
    success(f"All checks passed. {passed}/{total}.")
    return 0


# ── entry point ─────────────────────────────────────────────────────


@app.callback(invoke_without_command=True)
def doctor(
    typer_ctx: typer.Context,
    output: OutputFormat | None = typer.Option(
        None,
        "--output",
        "-o",
        help=(
            "Output format. 'json' emits the check array structured "
            "for monitoring scripts (each entry has name / ok / "
            "summary / detail). Default: human-friendly text report."
        ),
        case_sensitive=False,
    ),
) -> None:
    """Run a health check against the active context.

    Reports each step (config / context / API reachable / key
    status / IP whitelist / account) and exits 0 only if every
    check passed. ``--output json`` produces a structured report
    suitable for piping into monitoring scripts.

    Common failure modes the doctor catches:

    * Config file missing or empty → first-time setup hint.
    * API key invalid → rotate via your Impreza Account.
    * IP not whitelisted → add the IP or switch context.
    * Key paused / revoked → rotate.
    * Account-scope mismatch → contact support.
    """
    # If a subcommand was invoked, do nothing — Typer routes there.
    if typer_ctx.invoked_subcommand is not None:
        return

    state = from_typer_context(typer_ctx)
    fmt = resolve_output(state, output)

    results: list[_CheckResult] = []

    # Check 1: config + context (failures here exit before we can
    # build a Client; the parent helper renders its own error).
    results.append(_check_context(state))

    # Build the client. make_client_or_exit handles config errors
    # itself with a friendly stderr + Exit(1), so reaching this point
    # means we have a valid client.
    with make_client_or_exit(state) as client:
        key_holder: dict[str, Any] = {}
        results.append(_check_api_reachable(client, key_holder))
        results.append(_check_key_status(key_holder))
        results.append(_check_ip_whitelist(key_holder))
        # Account profile only makes sense if API was reachable.
        if key_holder.get("key") is not None:
            results.append(_check_account_profile(client))
        else:
            r = _CheckResult("account-profile")
            r.skipped("api-reachable failed; cannot test other endpoints")
            results.append(r)

    if fmt is OutputFormat.JSON:
        sys.stdout.write(
            json.dumps([r.as_dict() for r in results], indent=2, ensure_ascii=False)
        )
        sys.stdout.write("\n")
        # JSON consumers want exit-code-as-signal too.
        if any(r.ok is False for r in results):
            raise typer.Exit(code=1)
        return

    _print_text_report(results)
    exit_code = _print_summary(results)
    if exit_code != 0:
        # Don't suppress the report — raise after printing so the
        # user sees the full diagnostic AND gets the non-zero exit.
        # typer.Exit raises a SystemExit(exit_code) which click /
        # CliRunner translates to result.exit_code.
        raise typer.Exit(code=exit_code)
    # Some terminals expect a trailing newline before the prompt.
    # The summary printer already emitted one via secho.

    # Note: we don't use `error()` for failed checks because the
    # report itself contains the [FAIL] markers in red — adding a
    # stderr "Error:" line on top would just duplicate the signal.
