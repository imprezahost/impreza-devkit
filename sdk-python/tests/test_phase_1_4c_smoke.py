"""Live integration smoke tests for Phase 1.4c (hosting + email).

Read-only operations only. Mutating endpoints (`trigger_autossl` and
`setup_admin`) are covered by mocks — running them live requires
opt-in flags reserved for a future destructive smoke suite.

Each test resolves its target by querying ``c.account.services``
and skipping silently when no matching service exists. This keeps
the smokes runnable on accounts of any shape.

Run::

    export IMPREZA_API_KEY=imp_...
    export IMPREZA_API_SECRET=...
    pytest tests/test_phase_1_4c_smoke.py -v -s
"""

from __future__ import annotations

import pytest

from impreza import Client, TitanSsoUrl


def _first_service_id(client: Client, *, contains: str) -> int | None:
    """Return the ID of the first Active service whose product_group
    includes the given substring (case-insensitive). None if none match.
    """
    needle = contains.lower()
    for svc in client.account.services.list(status="Active"):
        group = (svc.product_group or "").lower()
        if needle in group:
            return svc.id
    return None


def _first_domain_with_titan(client: Client) -> str | None:
    for svc in client.account.services.list(status="Active"):
        if "titan" in (svc.product_group or "").lower() and svc.domain:
            return svc.domain
    return None


def _first_domain_with_workspace(client: Client) -> str | None:
    for svc in client.account.services.list(status="Active"):
        if "workspace" in (svc.product_group or "").lower() and svc.domain:
            return svc.domain
    return None


def test_smoke_hosting_get_round_trips(live_client: Client) -> None:
    """If the account has a Linux/cPanel hosting service, fetch its summary."""
    sid = _first_service_id(live_client, contains="hosting")
    if sid is None:
        pytest.skip("no hosting service on this account")

    info = live_client.hosting.get(sid)
    assert isinstance(info, dict)
    print(
        f"\n  hosting service {sid}: keys={sorted(info.keys())[:6]}"
        + ("..." if len(info) > 6 else "")
    )


def test_smoke_hosting_nameservers_lists(live_client: Client) -> None:
    sid = _first_service_id(live_client, contains="hosting")
    if sid is None:
        pytest.skip("no hosting service on this account")

    ns = live_client.hosting.nameservers(sid)
    assert isinstance(ns, list)
    for n in ns:
        assert isinstance(n, str) and n  # non-empty strings only
    print(f"\n  hosting service {sid}: {len(ns)} nameserver(s)")


def test_smoke_titan_dns_records_for_active_titan(live_client: Client) -> None:
    """Titan domains return their MX/SPF/DKIM records read-only."""
    domain = _first_domain_with_titan(live_client)
    if domain is None:
        pytest.skip("no Titan Email domain on this account")

    records = live_client.email.titan.dns_records(domain)
    assert isinstance(records, list)
    types = {str(r.get("type") or "").upper() for r in records if isinstance(r, dict)}
    print(f"\n  titan {domain}: {len(records)} record(s); types present={sorted(types)}")


def test_smoke_titan_sso_returns_sso_url(live_client: Client) -> None:
    domain = _first_domain_with_titan(live_client)
    if domain is None:
        pytest.skip("no Titan Email domain on this account")

    sso = live_client.email.titan.sso(domain)
    assert isinstance(sso, TitanSsoUrl)
    assert sso.sso_url.startswith("http")
    print(f"\n  titan {domain}: sso_url present (length={len(sso.sso_url)} chars)")


def test_smoke_workspace_dns_records_account_level(live_client: Client) -> None:
    """The Workspace MX records endpoint is account-level — runs as long as
    the client has at least one Workspace service."""
    domain = _first_domain_with_workspace(live_client)
    if domain is None:
        pytest.skip("no Google Workspace service on this account")

    records = live_client.email.google.dns_records()
    assert isinstance(records, list)
    print(
        f"\n  workspace (account-level): {len(records)} record(s)"
        f"; first Workspace domain on account: {domain}"
    )


def test_smoke_workspace_get_round_trips(live_client: Client) -> None:
    domain = _first_domain_with_workspace(live_client)
    if domain is None:
        pytest.skip("no Google Workspace service on this account")

    details = live_client.email.google.get(domain)
    assert isinstance(details, dict)
    print(f"\n  workspace {domain}: keys={sorted(details.keys())[:6]}")
