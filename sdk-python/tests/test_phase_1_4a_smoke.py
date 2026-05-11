"""Live integration smoke tests for Phase 1.4a (domains).

Only operations with **no side effects** run by default:

* ``c.domains.check([...])`` against a deliberately implausible domain
  name (zero risk of accidental registration).

Operations that mutate state (register, transfer, set_nameservers, lock,
unlock, dns add/update/delete, purchase_id_protection, resend_*) are
covered by the mocked unit suite. Real-world validation of those
requires a known test domain and is gated behind the
``IMPREZA_DESTRUCTIVE_TESTS=1`` env var (no destructive smoke is wired
up yet — pattern reserved for future use).

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_4a_smoke.py -v -s
"""

from __future__ import annotations

from impreza import Client


def test_smoke_domain_check_returns_availability(live_client: Client) -> None:
    """Check availability of one likely-available and one likely-taken domain.

    Probes:
    * ``imprezahost.com`` — taken (the company's own domain).
    * A pseudo-random ``zzz...`` domain that should be available.
    """
    available_probe = "zzz-impreza-sdk-smoke-9821641.com"
    result = live_client.domains.check(["imprezahost.com", available_probe])

    assert isinstance(result, dict)
    assert "imprezahost.com" in result
    assert "imprezahost.com" in result and result["imprezahost.com"] is False, (
        "imprezahost.com should be reported as not available"
    )
    assert available_probe in result
    assert result[available_probe] is True, (
        f"{available_probe} should be reported as available"
    )

    print(
        f"\n  imprezahost.com → taken={not result['imprezahost.com']}, "
        f"{available_probe} → available={result[available_probe]}"
    )
