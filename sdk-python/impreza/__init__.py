"""Impreza Host Python SDK.

Phase 1.4b-i alpha — sync :class:`Client` and async :class:`AsyncClient`,
both with auth, retry, error mapping, optional Tor routing, and the
following resources:

* ``account`` (with nested ``account.services``) — read
* ``catalog`` (products + groups + TLD pricing) — read
* ``invoices`` — read
* ``domains`` (with nested ``domains.dns``) — full CRUD (16 ops)
* ``vps`` — smart-dispatch over Proxmox + Cloud backends with the
  common operation surface (power, hostname, password, reinstall,
  status). Backend-specific ops (snapshots, backups, images, rescue,
  ISO) land in 1.4b-ii.

>>> from impreza import Client
>>> with Client.from_env() as c:
...     print(c.account.get().balance)
...     for svc in c.account.services.list(status="Active"):
...         print(svc.id, svc.product, svc.status)
...     vps = c.vps.get(1234)             # auto-dispatches Proxmox/Cloud
...     print(vps.status().power_state)
...     vps.reboot()

>>> import asyncio
>>> from impreza import AsyncClient
>>> async def main():
...     async with AsyncClient.from_env() as c:
...         for vps in await c.vps.list():
...             print(vps.id, vps.backend)
>>> asyncio.run(main())  # doctest: +SKIP
"""

from ._polling import AsyncOperation, Operation
from ._topup import AsyncTopupInvoice, TopupInvoice
from .async_client import AsyncClient
from .client import Client
from .exceptions import (
    ApiError,
    AuthError,
    BackendNotSupported,
    ImprezaError,
    InsufficientCredit,
    InvalidRequest,
    IpNotWhitelisted,
    NetworkError,
    OperationFailed,
    OperationTimeout,
    PermissionDenied,
    RateLimitExceeded,
    ResourceNotFound,
    ServerError,
    TopupFailed,
    TopupTimeout,
    UpstreamError,
    WebhookSignatureMismatch,
)
from .models.account import (
    AccountInfo,
    IpWhitelistEntry,
    KeyIdentity,
    TopupInvoiceData,
)
from .models.dns import DnsRecord
from .models.domain import Domain, DomainRegistration, DomainTransfer
from .models.email import TitanSsoUrl
from .models.invoice import Invoice, InvoiceDetail, InvoiceItem, InvoiceTransaction
from .models.order import Order, OrderDetail, OrderItem, OrderResult
from .models.product import (
    ConfigOption,
    ConfigOptionChoice,
    CustomField,
    CyclePrice,
    Product,
    ProductDetail,
    ProductGroup,
)
from .models.service import Service, VpsBackend
from .models.tld import TldPricing
from .models.vps import VpsStatus
from .models.vps_extras import (
    Backup,
    BackupSchedule,
    ConsoleUrl,
    Image,
    Snapshot,
    SshConsole,
    SshKey,
    VncCredentials,
    VpsOperation,
)
from .models.webhook import (
    WebhookDelivery,
    WebhookEvent,
    WebhookEventCatalog,
    WebhookSubscription,
)
from .resources.vps import AsyncVps, Vps

# Read the installed package version from metadata. This always matches
# the wheel that pip installed, so `impreza.__version__` and
# `pip show impreza-sdk` stay in sync without anyone remembering to
# bump a hard-coded string at release time.
from importlib.metadata import PackageNotFoundError, version as _pkg_version

try:
    __version__ = _pkg_version("impreza-sdk")
except PackageNotFoundError:  # source checkout without an install
    __version__ = "0.0.0+unknown"

del _pkg_version, PackageNotFoundError

__all__ = [
    "AccountInfo",
    "ApiError",
    "AsyncClient",
    "AsyncOperation",
    "AsyncTopupInvoice",
    "AsyncVps",
    "AuthError",
    "Backup",
    "BackupSchedule",
    "BackendNotSupported",
    "Client",
    "ConfigOption",
    "ConfigOptionChoice",
    "ConsoleUrl",
    "CustomField",
    "CyclePrice",
    "DnsRecord",
    "Domain",
    "DomainRegistration",
    "DomainTransfer",
    "Image",
    "ImprezaError",
    "InsufficientCredit",
    "InvalidRequest",
    "Invoice",
    "InvoiceDetail",
    "InvoiceItem",
    "InvoiceTransaction",
    "IpNotWhitelisted",
    "IpWhitelistEntry",
    "KeyIdentity",
    "NetworkError",
    "Operation",
    "OperationFailed",
    "OperationTimeout",
    "Order",
    "OrderDetail",
    "OrderItem",
    "OrderResult",
    "PermissionDenied",
    "Product",
    "ProductDetail",
    "ProductGroup",
    "RateLimitExceeded",
    "ResourceNotFound",
    "Service",
    "ServerError",
    "Snapshot",
    "SshConsole",
    "SshKey",
    "TitanSsoUrl",
    "TldPricing",
    "TopupFailed",
    "TopupInvoice",
    "TopupInvoiceData",
    "TopupTimeout",
    "UpstreamError",
    "VncCredentials",
    "Vps",
    "VpsBackend",
    "VpsOperation",
    "VpsStatus",
    "WebhookDelivery",
    "WebhookEvent",
    "WebhookEventCatalog",
    "WebhookSignatureMismatch",
    "WebhookSubscription",
    "__version__",
]
