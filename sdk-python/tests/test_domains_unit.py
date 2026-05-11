"""Unit tests for ``c.domains`` (sync + async).

Mocked via ``respx`` — no real API call.
"""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from impreza import (
    AsyncClient,
    Client,
    DnsRecord,
    Domain,
    DomainRegistration,
    DomainTransfer,
)

BASE = "https://api.imprezahost.com/v1"


# ── shared fixtures ────────────────────────────────────────────────────


def _check_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "availability": {
                "example.com": False,
                "example.net": True,
                "example.io": True,
            }
        },
        "meta": {"request_id": "req_test"},
    }


def _domain_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "domain": "example.com",
            "status": "Active",
            "registration_date": "2024-06-01",
            "expires_at": "2027-06-01",
            "nameservers": ["ns1.imprezahost.com", "ns2.imprezahost.com"],
            "lock_status": True,
            "id_protection": False,
        },
        "meta": {"request_id": "req_test"},
    }


def _registration_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "order_id": 1234,
            "invoice_id": 5678,
            "domain": "mynewdomain.com",
            "years": 1,
            "amount": 12.99,
            "currency": "USD",
            "status": "Registered",
            "message": "Domain registration order created and paid from balance.",
        },
        "meta": {"request_id": "req_test"},
    }


def _transfer_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "order_id": 1235,
            "invoice_id": 5679,
            "domain": "existing.com",
            "years": 1,
            "amount": 12.99,
            "currency": "USD",
            "message": "Transfer order created.",
        },
        "meta": {"request_id": "req_test"},
    }


def _dns_list_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {
            "records": [
                {"type": "A", "host": "@", "value": "185.100.86.42", "ttl": 14400},
                {
                    "type": "MX",
                    "host": "@",
                    "value": "mx.example.com",
                    "ttl": 14400,
                    "priority": 10,
                },
                {"type": "TXT", "host": "_dmarc", "value": "v=DMARC1; p=none", "ttl": 3600},
            ],
        },
        "meta": {"request_id": "req_test"},
    }


def _empty_payload() -> dict[str, object]:
    return {"success": True, "data": {}, "meta": {"request_id": "req_test"}}


def _epp_payload() -> dict[str, object]:
    return {
        "success": True,
        "data": {"epp_code": "abc123xyz"},
        "meta": {"request_id": "req_test"},
    }


# ── check / get ────────────────────────────────────────────────────────


@respx.mock
def test_domains_check_parses_availability() -> None:
    route = respx.get(f"{BASE}/domains/check").mock(
        return_value=httpx.Response(200, json=_check_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.domains.check(["example.com", "example.net", "example.io"])

    assert result == {"example.com": False, "example.net": True, "example.io": True}
    sent = route.calls.last.request
    assert sent.url.params.get("domains") == "example.com,example.net,example.io"


def test_domains_check_empty_raises() -> None:
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(ValueError, match="must not be empty"),
    ):
        c.domains.check([])


def test_domains_check_too_many_raises() -> None:
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(ValueError, match="at most 10"),
    ):
        c.domains.check([f"d{i}.com" for i in range(11)])


@respx.mock
def test_domains_get_parses_detail() -> None:
    respx.get(f"{BASE}/domains/example.com").mock(
        return_value=httpx.Response(200, json=_domain_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        domain = c.domains.get("example.com")

    assert isinstance(domain, Domain)
    assert domain.domain == "example.com"
    assert domain.status == "Active"
    assert domain.lock_status is True
    assert domain.nameservers == ["ns1.imprezahost.com", "ns2.imprezahost.com"]


@respx.mock
def test_domains_get_falls_back_to_url_when_response_omits_domain_field() -> None:
    """Phase 2.4 live smoke uncovered that ``GET /domains/{domain}``
    sometimes returns a payload without the ``domain`` key (server
    forwarding raw ResellerClub fields like ``domainname`` instead).
    The SDK must inject the URL parameter as a fallback so model
    validation succeeds, regardless of which server build is on the
    other end.
    """
    respx.get(f"{BASE}/domains/imprezahost.icu").mock(
        return_value=httpx.Response(
            200,
            json={
                "success": True,
                "data": {
                    # No "domain" key. ResellerClub-style raw fields:
                    "domainname": "imprezahost.icu",
                    "currentstatus": "Active",
                    "creationtime": "1774918678",
                    "endtime": "1838159999",
                },
                "meta": {"request_id": "req_t"},
            },
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        domain = c.domains.get("imprezahost.icu")

    # The fallback supplied the missing field from the URL parameter.
    assert isinstance(domain, Domain)
    assert domain.domain == "imprezahost.icu"
    # The other documented fields are absent, so optional values stay None.
    # extra="ignore" on the model means ResellerClub-specific keys
    # (domainname, currentstatus, creationtime, endtime) are silently
    # dropped without breaking validation.
    assert domain.status is None
    assert domain.expires_at is None


# ── register / transfer ────────────────────────────────────────────────


@respx.mock
def test_domains_register_sends_full_body() -> None:
    route = respx.post(f"{BASE}/domains/register").mock(
        return_value=httpx.Response(201, json=_registration_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.domains.register(
            domain="mynewdomain.com",
            years=1,
            nameservers=["ns1.imprezahost.com", "ns2.imprezahost.com"],
        )

    assert isinstance(result, DomainRegistration)
    assert result.order_id == 1234
    assert result.invoice_id == 5678
    body = json.loads(route.calls.last.request.content)
    assert body == {
        "domain": "mynewdomain.com",
        "years": 1,
        "nameservers": ["ns1.imprezahost.com", "ns2.imprezahost.com"],
    }


@respx.mock
def test_domains_register_omits_nameservers_when_not_given() -> None:
    route = respx.post(f"{BASE}/domains/register").mock(
        return_value=httpx.Response(201, json=_registration_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.register(domain="mynewdomain.com", years=1)

    body = json.loads(route.calls.last.request.content)
    assert "nameservers" not in body
    assert body == {"domain": "mynewdomain.com", "years": 1}


@respx.mock
def test_domains_transfer_sends_epp_code() -> None:
    route = respx.post(f"{BASE}/domains/transfer").mock(
        return_value=httpx.Response(201, json=_transfer_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.domains.transfer(domain="existing.com", epp_code="abc123xyz", years=1)

    assert isinstance(result, DomainTransfer)
    body = json.loads(route.calls.last.request.content)
    assert body == {"domain": "existing.com", "epp_code": "abc123xyz", "years": 1}


# ── nameservers / lock ─────────────────────────────────────────────────


@respx.mock
def test_domains_set_nameservers_uses_put() -> None:
    route = respx.put(f"{BASE}/domains/example.com/nameservers").mock(
        return_value=httpx.Response(200, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.set_nameservers(
            "example.com", ["ns1.cloudflare.com", "ns2.cloudflare.com"]
        )

    assert route.called
    body = json.loads(route.calls.last.request.content)
    assert body == {"nameservers": ["ns1.cloudflare.com", "ns2.cloudflare.com"]}


def test_domains_set_nameservers_min_two_validated() -> None:
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(ValueError, match="at least 2"),
    ):
        c.domains.set_nameservers("example.com", ["ns1.foo.com"])


@respx.mock
def test_domains_lock_posts_no_body() -> None:
    route = respx.post(f"{BASE}/domains/example.com/lock").mock(
        return_value=httpx.Response(200, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.lock("example.com")

    assert route.called


@respx.mock
def test_domains_unlock_returns_epp_code() -> None:
    respx.delete(f"{BASE}/domains/example.com/lock").mock(
        return_value=httpx.Response(200, json=_epp_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        epp = c.domains.unlock("example.com")

    assert epp == "abc123xyz"


# ── DNS ────────────────────────────────────────────────────────────────


@respx.mock
def test_dns_list_parses_records() -> None:
    respx.get(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_dns_list_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        records = c.domains.dns.list("example.com")

    assert len(records) == 3
    assert all(isinstance(r, DnsRecord) for r in records)
    mx = next(r for r in records if r.type == "MX")
    assert mx.priority == 10
    assert mx.value == "mx.example.com"


@respx.mock
def test_dns_add_sends_correct_body() -> None:
    route = respx.post(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(201, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.dns.add(
            "example.com", type="A", host="www", value="1.2.3.4", ttl=3600
        )

    body = json.loads(route.calls.last.request.content)
    assert body == {"type": "A", "host": "www", "value": "1.2.3.4", "ttl": 3600}


@respx.mock
def test_dns_add_includes_priority_for_mx() -> None:
    route = respx.post(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(201, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.dns.add(
            "example.com",
            type="MX",
            host="@",
            value="mx.example.com",
            priority=10,
        )

    body = json.loads(route.calls.last.request.content)
    assert body["priority"] == 10


def test_dns_add_invalid_type_raises() -> None:
    with (
        Client(api_key="x", api_secret="y") as c,
        pytest.raises(ValueError, match="Invalid DNS record type"),
    ):
        c.domains.dns.add("example.com", type="HTTPS", host="@", value="x")


@respx.mock
def test_dns_update_uses_put_with_old_new_value() -> None:
    route = respx.put(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.dns.update(
            "example.com",
            type="A",
            host="@",
            old_value="1.2.3.4",
            new_value="5.6.7.8",
        )

    body = json.loads(route.calls.last.request.content)
    assert body == {
        "type": "A",
        "host": "@",
        "old_value": "1.2.3.4",
        "new_value": "5.6.7.8",
    }


@respx.mock
def test_dns_delete_sends_record_in_body() -> None:
    route = respx.delete(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.dns.delete("example.com", type="A", host="@", value="1.2.3.4")

    body = json.loads(route.calls.last.request.content)
    assert body == {"type": "A", "host": "@", "value": "1.2.3.4"}


# ── activate / id-protection / resend trio ────────────────────────────


@respx.mock
def test_activate_dns_posts_to_activate_endpoint() -> None:
    route = respx.post(f"{BASE}/domains/example.com/dns/activate").mock(
        return_value=httpx.Response(200, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        c.domains.activate_dns("example.com")

    assert route.called


@respx.mock
def test_purchase_id_protection_returns_data_dict() -> None:
    respx.post(f"{BASE}/domains/example.com/id-protection").mock(
        return_value=httpx.Response(
            201,
            json={
                "success": True,
                "data": {"invoice_id": 9999, "amount": 9.0, "currency": "USD"},
                "meta": {"request_id": "req_test"},
            },
        )
    )

    with Client(api_key="x", api_secret="y") as c:
        result = c.domains.purchase_id_protection("example.com")

    assert result == {"invoice_id": 9999, "amount": 9.0, "currency": "USD"}


@respx.mock
@pytest.mark.parametrize(
    ("method_name", "path"),
    [
        ("resend_raa_verification", "/domains/example.com/raa-verify"),
        ("resend_gdpr_auth", "/domains/example.com/gdpr-auth"),
        ("resend_transfer_approval", "/domains/example.com/transfer-approval"),
    ],
)
def test_resend_email_endpoints(method_name: str, path: str) -> None:
    route = respx.post(f"{BASE}{path}").mock(
        return_value=httpx.Response(200, json=_empty_payload())
    )

    with Client(api_key="x", api_secret="y") as c:
        getattr(c.domains, method_name)("example.com")

    assert route.called


# ── async wiring ───────────────────────────────────────────────────────


@pytest.mark.asyncio
@respx.mock
async def test_async_domains_check() -> None:
    respx.get(f"{BASE}/domains/check").mock(
        return_value=httpx.Response(200, json=_check_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        result = await c.domains.check(["example.com"])

    assert result["example.com"] is False


@pytest.mark.asyncio
@respx.mock
async def test_async_domains_get() -> None:
    respx.get(f"{BASE}/domains/example.com").mock(
        return_value=httpx.Response(200, json=_domain_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        domain = await c.domains.get("example.com")

    assert domain.lock_status is True


@pytest.mark.asyncio
@respx.mock
async def test_async_dns_list() -> None:
    respx.get(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_dns_list_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        records = await c.domains.dns.list("example.com")

    assert len(records) == 3


@pytest.mark.asyncio
@respx.mock
async def test_async_dns_update_uses_put() -> None:
    route = respx.put(f"{BASE}/domains/example.com/dns").mock(
        return_value=httpx.Response(200, json=_empty_payload())
    )

    async with AsyncClient(api_key="x", api_secret="y") as c:
        await c.domains.dns.update(
            "example.com",
            type="A",
            host="@",
            old_value="1.2.3.4",
            new_value="5.6.7.8",
        )

    assert route.called
