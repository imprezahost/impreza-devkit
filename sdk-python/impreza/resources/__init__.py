"""Resource classes — accessed via members of :class:`impreza.Client`
and :class:`impreza.AsyncClient`."""

from .account import AccountResource, AsyncAccountResource
from .catalog import AsyncCatalogResource, CatalogResource
from .domains import (
    AsyncDnsResource,
    AsyncDomainsResource,
    DnsResource,
    DomainsResource,
)
from .email import (
    AsyncEmailResource,
    AsyncGoogleWorkspaceResource,
    AsyncTitanResource,
    EmailResource,
    GoogleWorkspaceResource,
    TitanResource,
)
from .hosting import AsyncHostingResource, HostingResource
from .invoices import AsyncInvoicesResource, InvoicesResource
from .orders import AsyncOrdersResource, OrdersResource
from .services import AsyncServicesResource, ServicesResource
from .vps import AsyncVps, AsyncVpsResource, Vps, VpsResource
from .vps_cloud import (
    AsyncCloudImagesResource,
    AsyncCloudIsoResource,
    AsyncCloudRdnsResource,
    AsyncCloudRescueResource,
    AsyncCloudSshKeysResource,
    CloudImagesResource,
    CloudIsoResource,
    CloudRdnsResource,
    CloudRescueResource,
    CloudSshKeysResource,
)
from .vps_proxmox import (
    AsyncProxmoxBackupSchedulesResource,
    AsyncProxmoxBackupsResource,
    AsyncProxmoxOperationsResource,
    AsyncProxmoxSnapshotsResource,
    ProxmoxBackupSchedulesResource,
    ProxmoxBackupsResource,
    ProxmoxOperationsResource,
    ProxmoxSnapshotsResource,
)
from .webhooks import AsyncWebhooksResource, WebhooksResource

__all__ = [
    "AccountResource",
    "AsyncAccountResource",
    "AsyncCatalogResource",
    "AsyncCloudImagesResource",
    "AsyncCloudIsoResource",
    "AsyncCloudRdnsResource",
    "AsyncCloudRescueResource",
    "AsyncCloudSshKeysResource",
    "AsyncDnsResource",
    "AsyncDomainsResource",
    "AsyncEmailResource",
    "AsyncGoogleWorkspaceResource",
    "AsyncHostingResource",
    "AsyncInvoicesResource",
    "AsyncOrdersResource",
    "AsyncProxmoxBackupSchedulesResource",
    "AsyncProxmoxBackupsResource",
    "AsyncProxmoxOperationsResource",
    "AsyncProxmoxSnapshotsResource",
    "AsyncServicesResource",
    "AsyncTitanResource",
    "AsyncVps",
    "AsyncVpsResource",
    "AsyncWebhooksResource",
    "CatalogResource",
    "CloudImagesResource",
    "CloudIsoResource",
    "CloudRdnsResource",
    "CloudRescueResource",
    "CloudSshKeysResource",
    "DnsResource",
    "DomainsResource",
    "EmailResource",
    "GoogleWorkspaceResource",
    "HostingResource",
    "InvoicesResource",
    "OrdersResource",
    "ProxmoxBackupSchedulesResource",
    "ProxmoxBackupsResource",
    "ProxmoxOperationsResource",
    "ProxmoxSnapshotsResource",
    "ServicesResource",
    "TitanResource",
    "Vps",
    "VpsResource",
    "WebhooksResource",
]
