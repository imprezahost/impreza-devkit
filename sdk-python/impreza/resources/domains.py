"""Domains resource — accessed via ``Client.domains`` and ``AsyncClient.domains``.

Phase 1.4a wires up all 16 domain operations from the API: availability,
registration, transfer, lock/unlock, nameservers, DNS CRUD, and the
"resend email" trio (RAA, GDPR, transfer-approval). DNS operations live
on the nested ``c.domains.dns`` sub-resource.

Sync and async variants share extractor helpers and a common parameter
validator at the top of the module — only the I/O surface differs.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from ..models.dns import DnsRecord
from ..models.domain import Domain, DomainRegistration, DomainTransfer

if TYPE_CHECKING:  # pragma: no cover
    from .._http import HttpClient
    from .._http_async import AsyncHttpClient

DnsRecordType = str  # one of: A, AAAA, CNAME, MX, TXT, NS, SRV
_VALID_DNS_TYPES = {"A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV"}


# ── extractors ─────────────────────────────────────────────────────────


def _data(payload: dict[str, object]) -> dict[str, object]:
    raw = payload.get("data")
    return raw if isinstance(raw, dict) else {}


def _extract_availability(payload: dict[str, object]) -> dict[str, bool]:
    data = _data(payload)
    raw = data.get("availability")
    if not isinstance(raw, dict):
        return {}
    return {str(k): bool(v) for k, v in raw.items()}


def _extract_domain(
    payload: dict[str, object],
    *,
    requested_domain: str | None = None,
) -> Domain:
    """Validate the upstream `/domains/{domain}` response into a
    :class:`Domain`.

    The server is expected to return the field set documented in
    ``openapi.yaml`` (``domain``, ``status``, ``expires_at``, etc.),
    but historically forwarded the raw ResellerClub upstream payload
    where the domain name lives under ``domainname`` (no ``domain``
    field). Pass ``requested_domain`` (the URL path parameter) so we
    can inject it as a fallback — that keeps the SDK robust whether
    or not the server is on a normalised build.
    """
    data = _data(payload)
    if requested_domain and "domain" not in data:
        data = {**data, "domain": requested_domain}
    return Domain.model_validate(data)


def _extract_registration(payload: dict[str, object]) -> DomainRegistration:
    return DomainRegistration.model_validate(_data(payload))


def _extract_transfer(payload: dict[str, object]) -> DomainTransfer:
    return DomainTransfer.model_validate(_data(payload))


def _extract_dns_records(payload: dict[str, object]) -> list[DnsRecord]:
    data = _data(payload)
    raw = data.get("records")
    items = raw if isinstance(raw, list) else []
    return [DnsRecord.model_validate(item) for item in items]


def _extract_epp_code(payload: dict[str, object]) -> str:
    data = _data(payload)
    epp = data.get("epp_code")
    if not isinstance(epp, str):
        # Defensive — the contract is documented to include epp_code.
        return ""
    return epp


# ── parameter helpers ──────────────────────────────────────────────────


def _validate_record_type(record_type: str) -> None:
    if record_type not in _VALID_DNS_TYPES:
        raise ValueError(
            f"Invalid DNS record type {record_type!r}; "
            f"must be one of {sorted(_VALID_DNS_TYPES)}"
        )


def _check_params(domains: list[str]) -> dict[str, object]:
    if not domains:
        raise ValueError("domains list must not be empty")
    if len(domains) > 10:
        raise ValueError("API accepts at most 10 domains per check call")
    return {"domains": ",".join(domains)}


def _register_body(
    domain: str, years: int, nameservers: list[str] | None
) -> dict[str, object]:
    body: dict[str, object] = {"domain": domain, "years": years}
    if nameservers is not None:
        body["nameservers"] = nameservers
    return body


def _transfer_body(domain: str, epp_code: str, years: int) -> dict[str, object]:
    return {"domain": domain, "epp_code": epp_code, "years": years}


def _dns_add_body(
    record_type: str,
    host: str,
    value: str,
    ttl: int | None,
    priority: int | None,
) -> dict[str, object]:
    _validate_record_type(record_type)
    body: dict[str, object] = {"type": record_type, "host": host, "value": value}
    if ttl is not None:
        body["ttl"] = ttl
    if priority is not None:
        body["priority"] = priority
    return body


def _dns_update_body(
    record_type: str,
    host: str,
    old_value: str,
    new_value: str,
    ttl: int | None,
    priority: int | None,
) -> dict[str, object]:
    _validate_record_type(record_type)
    body: dict[str, object] = {
        "type": record_type,
        "host": host,
        "old_value": old_value,
        "new_value": new_value,
    }
    if ttl is not None:
        body["ttl"] = ttl
    if priority is not None:
        body["priority"] = priority
    return body


def _dns_delete_body(record_type: str, host: str, value: str) -> dict[str, object]:
    _validate_record_type(record_type)
    return {"type": record_type, "host": host, "value": value}


# ── sync ───────────────────────────────────────────────────────────────


class DnsResource:
    """Sync DNS record operations under ``c.domains.dns``."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self, domain: str) -> list[DnsRecord]:
        """List all DNS records on ``domain``."""
        payload = self._http.get(f"/domains/{domain}/dns")
        return _extract_dns_records(payload)

    def add(
        self,
        domain: str,
        *,
        type: DnsRecordType,
        host: str,
        value: str,
        ttl: int | None = None,
        priority: int | None = None,
    ) -> None:
        """Add a record. ``priority`` is required for ``MX``."""
        self._http.post(
            f"/domains/{domain}/dns",
            json=_dns_add_body(type, host, value, ttl, priority),
        )

    def update(
        self,
        domain: str,
        *,
        type: DnsRecordType,
        host: str,
        old_value: str,
        new_value: str,
        ttl: int | None = None,
        priority: int | None = None,
    ) -> None:
        """Update a record by matching ``type + host + old_value``."""
        self._http.put(
            f"/domains/{domain}/dns",
            json=_dns_update_body(type, host, old_value, new_value, ttl, priority),
        )

    def delete(
        self,
        domain: str,
        *,
        type: DnsRecordType,
        host: str,
        value: str,
    ) -> None:
        """Delete a record by exact match of ``type + host + value``."""
        self._http.delete(
            f"/domains/{domain}/dns",
            json=_dns_delete_body(type, host, value),
        )


class DomainsResource:
    """Sync operations on registered + about-to-register domains."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http
        self.dns = DnsResource(http)

    # ── catalog-ish reads ──────────────────────────────────────────────

    def check(self, domains: list[str]) -> dict[str, bool]:
        """Check availability of up to 10 domains in one call.

        Returns a dict mapping each domain string to ``True`` (available)
        or ``False`` (taken).
        """
        payload = self._http.get("/domains/check", params=_check_params(domains))
        return _extract_availability(payload)

    def get(self, domain: str) -> Domain:
        """Return full domain detail (status, expiry, NS, lock, EPP, privacy)."""
        payload = self._http.get(f"/domains/{domain}")
        return _extract_domain(payload, requested_domain=domain)

    # ── ordering ───────────────────────────────────────────────────────

    def register(
        self,
        *,
        domain: str,
        years: int,
        nameservers: list[str] | None = None,
    ) -> DomainRegistration:
        """Register a new domain. Pays from account balance."""
        payload = self._http.post(
            "/domains/register",
            json=_register_body(domain, years, nameservers),
        )
        return _extract_registration(payload)

    def transfer(
        self,
        *,
        domain: str,
        epp_code: str,
        years: int = 1,
    ) -> DomainTransfer:
        """Transfer a domain in. Requires the EPP/auth code from current registrar."""
        payload = self._http.post(
            "/domains/transfer",
            json=_transfer_body(domain, epp_code, years),
        )
        return _extract_transfer(payload)

    # ── domain config ──────────────────────────────────────────────────

    def set_nameservers(self, domain: str, nameservers: list[str]) -> None:
        """Replace nameservers on the domain. Minimum 2 servers."""
        if len(nameservers) < 2:
            raise ValueError("at least 2 nameservers are required")
        self._http.put(
            f"/domains/{domain}/nameservers",
            json={"nameservers": nameservers},
        )

    def lock(self, domain: str) -> None:
        """Enable transfer lock."""
        self._http.post(f"/domains/{domain}/lock")

    def unlock(self, domain: str) -> str:
        """Disable transfer lock and return the EPP/auth code."""
        payload = self._http.delete(f"/domains/{domain}/lock")
        return _extract_epp_code(payload)

    def activate_dns(self, domain: str) -> None:
        """Activate DNS management — required before adding/editing records."""
        self._http.post(f"/domains/{domain}/dns/activate")

    def purchase_id_protection(self, domain: str) -> dict[str, object]:
        """Purchase WHOIS Privacy. Pays from account balance.

        Returns the raw ``data`` dict (typically ``{invoice_id, amount, ...}``).
        """
        payload = self._http.post(f"/domains/{domain}/id-protection")
        return _data(payload)

    # ── re-send notification emails ────────────────────────────────────

    def resend_raa_verification(self, domain: str) -> None:
        """Resend the RAA verification email."""
        self._http.post(f"/domains/{domain}/raa-verify")

    def resend_gdpr_auth(self, domain: str) -> None:
        """Resend the GDPR authorization email."""
        self._http.post(f"/domains/{domain}/gdpr-auth")

    def resend_transfer_approval(self, domain: str) -> None:
        """Resend the transfer approval email."""
        self._http.post(f"/domains/{domain}/transfer-approval")


# ── async ──────────────────────────────────────────────────────────────


class AsyncDnsResource:
    """Async DNS record operations under ``c.domains.dns``."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self, domain: str) -> list[DnsRecord]:
        payload = await self._http.get(f"/domains/{domain}/dns")
        return _extract_dns_records(payload)

    async def add(
        self,
        domain: str,
        *,
        type: DnsRecordType,
        host: str,
        value: str,
        ttl: int | None = None,
        priority: int | None = None,
    ) -> None:
        await self._http.post(
            f"/domains/{domain}/dns",
            json=_dns_add_body(type, host, value, ttl, priority),
        )

    async def update(
        self,
        domain: str,
        *,
        type: DnsRecordType,
        host: str,
        old_value: str,
        new_value: str,
        ttl: int | None = None,
        priority: int | None = None,
    ) -> None:
        await self._http.put(
            f"/domains/{domain}/dns",
            json=_dns_update_body(type, host, old_value, new_value, ttl, priority),
        )

    async def delete(
        self,
        domain: str,
        *,
        type: DnsRecordType,
        host: str,
        value: str,
    ) -> None:
        await self._http.delete(
            f"/domains/{domain}/dns",
            json=_dns_delete_body(type, host, value),
        )


class AsyncDomainsResource:
    """Async operations on registered + about-to-register domains."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http
        self.dns = AsyncDnsResource(http)

    async def check(self, domains: list[str]) -> dict[str, bool]:
        payload = await self._http.get("/domains/check", params=_check_params(domains))
        return _extract_availability(payload)

    async def get(self, domain: str) -> Domain:
        payload = await self._http.get(f"/domains/{domain}")
        return _extract_domain(payload, requested_domain=domain)

    async def register(
        self,
        *,
        domain: str,
        years: int,
        nameservers: list[str] | None = None,
    ) -> DomainRegistration:
        payload = await self._http.post(
            "/domains/register",
            json=_register_body(domain, years, nameservers),
        )
        return _extract_registration(payload)

    async def transfer(
        self,
        *,
        domain: str,
        epp_code: str,
        years: int = 1,
    ) -> DomainTransfer:
        payload = await self._http.post(
            "/domains/transfer",
            json=_transfer_body(domain, epp_code, years),
        )
        return _extract_transfer(payload)

    async def set_nameservers(self, domain: str, nameservers: list[str]) -> None:
        if len(nameservers) < 2:
            raise ValueError("at least 2 nameservers are required")
        await self._http.put(
            f"/domains/{domain}/nameservers",
            json={"nameservers": nameservers},
        )

    async def lock(self, domain: str) -> None:
        await self._http.post(f"/domains/{domain}/lock")

    async def unlock(self, domain: str) -> str:
        payload = await self._http.delete(f"/domains/{domain}/lock")
        return _extract_epp_code(payload)

    async def activate_dns(self, domain: str) -> None:
        await self._http.post(f"/domains/{domain}/dns/activate")

    async def purchase_id_protection(self, domain: str) -> dict[str, object]:
        payload = await self._http.post(f"/domains/{domain}/id-protection")
        return _data(payload)

    async def resend_raa_verification(self, domain: str) -> None:
        await self._http.post(f"/domains/{domain}/raa-verify")

    async def resend_gdpr_auth(self, domain: str) -> None:
        await self._http.post(f"/domains/{domain}/gdpr-auth")

    async def resend_transfer_approval(self, domain: str) -> None:
        await self._http.post(f"/domains/{domain}/transfer-approval")
