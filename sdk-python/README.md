# `impreza-sdk` — Official Python SDK for Impreza Host

Type-safe, sync + async client for the Impreza Host public REST API
([api.imprezahost.com](https://api.imprezahost.com)). Covers domains,
hosting (cPanel), managed email (Titan / Workspace), VPS (Proxmox +
Cloud) with smart dispatch, orders, invoices, webhooks, and
crypto top-up — all behind one client object.

```bash
pip install impreza-sdk
```

Requires Python 3.10+. See [`CHANGELOG.md`](../CHANGELOG.md) for the
release history.

## Quickstart

```python
from impreza import Client

# Reads IMPREZA_API_KEY and IMPREZA_API_SECRET from the environment.
# Passing them explicitly works too: Client(api_key="...", api_secret="...").
with Client.from_env() as c:
    me = c.account.get()
    print(me.balance, me.currency)

    # List active services across all backends
    for svc in c.account.services.list(status="Active"):
        print(svc.id, svc.product, svc.status, svc.vps_backend)
```

## Authentication

Two headers, no signing:

- `IMPREZA_API_KEY` — `imp_` + 40 hex chars (public identifier)
- `IMPREZA_API_SECRET` — 64 hex chars (shown once at creation)

Generate keys in your Impreza Account. Add the source IP of every
machine that calls the API to the key's IP whitelist.

```bash
export IMPREZA_API_KEY="imp_..."
export IMPREZA_API_SECRET="..."
```

## Crypto top-up

Top up account balance with BTC / XMR / TRX / USDT-TRC20 via the
existing `btcpayinline` gateway:

```python
invoice = c.account.topup(amount=50, method="xmr")
print(invoice.payment_url)        # btcpayinline URL
print(invoice.expires_at)         # ISO timestamp, 2h from creation

# Poll until paid (or fail on cancellation / refund / expiry)
invoice.wait_until_paid(timeout=7200, poll_interval=30)
print(invoice.balance_after)
```

For push delivery instead of polling, subscribe a webhook to the
`topup.paid` event (see below).

## Domains

```python
# Availability
print(c.domains.check(["example.com", "example.net"]))

# Lookup
domain = c.domains.get("example.com")
print(domain.status, domain.expiry_date)

# DNS CRUD (nested under domains)
records = c.domains.dns.list("example.com")
c.domains.dns.add("example.com", type="A", name="@", value="1.2.3.4", ttl=3600)
c.domains.dns.update("example.com", type="A", name="@",
                     old_value="1.2.3.4", new_value="5.6.7.8")
```

## VPS (smart dispatch)

One `c.vps.get(service_id)` call returns a backend-aware bound model.
The SDK looks up which backend (Proxmox or Cloud) the service is on
and routes operations transparently:

```python
vps = c.vps.get(17988)             # service id from c.account.services.list()
print(vps.backend)                 # "proxmox" or "cloud"

# Common surface — same on both backends
status = vps.status()
print(status.power_state)          # "running" (Proxmox) / "online" (Cloud)
vps.reboot()

# Backend-specific sub-resources (Proxmox)
vps.snapshots.list()
op = vps.snapshots.rollback("pre-update")  # returns Operation future
op.wait(timeout=600)               # blocks until queue completes

# Backend-specific sub-resources (Cloud)
vps.images.list()                  # account-scoped
vps.rescue.enable()
```

Wrong-backend access raises `BackendNotSupported` client-side — no
network call:

```python
vps = c.vps.get(17987)             # Cloud VPS
vps.snapshots.list()
# raises BackendNotSupported("snapshots is not supported on the 'cloud'
# VPS backend. Use vps.images instead.")
```

## Webhooks

The wire contract for webhook events is documented in
[`../openapi/asyncapi.yaml`](../openapi/asyncapi.yaml) (AsyncAPI 3.0)
— 16 event types across 7 domains (`webhook`, `topup`, `invoice`,
`order`, `service`, `domain`, `vps`) plus wildcards `*`, `vps.*`,
`domain.*`.

Subscribe to events server-side:

```python
sub = c.webhooks.create(
    url="https://example.com/hooks/impreza",
    events=["topup.paid", "vps.power_state_changed", "domain.*"],
)
print(sub.secret)                  # 64 hex chars; shown ONCE — store it
```

Verify deliveries on your receiver (timing-safe HMAC-SHA256):

```python
from impreza.webhooks import verify_signature, WebhookSignatureMismatch

# Inside your Flask / FastAPI / Django handler
try:
    event = verify_signature(
        body=request.body,                  # raw bytes, NOT a re-serialized JSON
        signature_header=request.headers["X-Impreza-Signature"],
        secret=os.environ["IMPREZA_WEBHOOK_SECRET"],
    )
except WebhookSignatureMismatch:
    return ("invalid signature", 401)

if event.type == "topup.paid":
    credit_user(event.data.client_id, event.data.amount)
```

## Async

Every resource has an async counterpart via `AsyncClient`. **Reach for it
when you need high-fanout calls** — provisioning many VPSs, polling a
bulk of invoices in parallel, etc. For one-shot scripts and interactive
use the sync `Client` is simpler and has the same surface.

```python
import asyncio
from impreza import AsyncClient

async def main():
    async with AsyncClient.from_env() as c:
        # Read calls fan out trivially:
        me = await c.account.get()

        # Concurrent reboot across every VPS — gather() returns when
        # all complete (or any one raises). 10 calls take ~one
        # round-trip's worth of wall-clock, not 10x.
        vpss = await c.vps.list()
        await asyncio.gather(*[v.reboot() for v in vpss])

        # Async futures work the same way as their sync counterparts:
        op = await c.vps.get(17988).snapshots.rollback("pre-update")
        await op.wait(timeout=600)         # AsyncOperation.wait()

        invoice = await c.account.topup(amount=50, method="xmr")
        await invoice.wait_until_paid(timeout=7200)  # AsyncTopupInvoice

asyncio.run(main())
```

The `impreza` CLI is **sync by design** — CLI invocations are one-shot
processes where the async-fanout speedup doesn't apply. The async path
exists for library users; everything the CLI does has a `Client.*`
equivalent. If you're integrating into a long-running async app
(FastAPI, an event consumer, a batch worker), import the SDK directly
and skip the CLI.

## Tor routing

```python
# Explicit
c = Client(api_key="...", api_secret="...",
           proxy="socks5://127.0.0.1:9050")

# Opt-in via flag or env var
c = Client.from_env(use_tor=True)        # equivalent to IMPREZA_USE_TOR=1

# Probe Tor; fall back to clearnet if not running
c = Client.from_env(auto_tor=True)
```

Backed by `httpx[socks]`. Sync and async parity. The probe used by
`auto_tor=True` never raises — failure means clearnet.

## Error handling

All errors subclass `ImprezaError`, so a single `except` catches
everything the SDK can throw:

```python
from impreza import (
    Client, ImprezaError,
    InsufficientCredit, RateLimitExceeded, IpNotWhitelisted,
    OperationFailed, TopupFailed,
)

try:
    c.orders.create(product_id=12, billing_cycle="monthly", domain="example.com")
except InsufficientCredit as e:
    needed = e.details.get("amount_needed")
    print(f"Need ${needed} more — opening a top-up invoice")
    inv = c.account.topup(amount=needed, method="btc")
    print(inv.payment_url)
except RateLimitExceeded as e:
    print(f"Rate limited — retry after {e.retry_after}s")
except IpNotWhitelisted:
    print("Your current IP isn't on the API key's whitelist")
```

## Action polling

Long-running Proxmox operations return `Operation` futures:

```python
op = c.vps.get(17988).backups.create()
print(op.uuid, op.status)          # 'queued' initially
op.wait(timeout=1800, poll_interval=10)
print(op.is_success(), op.finished_at)
```

`wait()` raises `OperationTimeout` if it doesn't finish, or
`OperationFailed` on a terminal failure state.

## Development

```bash
git clone https://github.com/imprezahost/impreza-devkit.git
cd impreza-devkit/sdk-python
python -m venv .venv
# Linux/macOS: source .venv/bin/activate
# Windows PowerShell: .venv\Scripts\Activate.ps1
pip install -e ".[test,dev]"

pytest                              # 266 unit tests + mocks
ruff check
mypy --strict impreza
```

To run the live integration smokes:

```bash
export IMPREZA_API_KEY="imp_..."
export IMPREZA_API_SECRET="..."
# Optional — only needed for the full topup decode smoke
export IMPREZA_TOPUP_INVOICE_ID="<id of an existing AddFunds invoice>"

pytest -v -s tests/
```

The destructive tests (real top-up creation, real Proxmox backup
creation) are gated behind `IMPREZA_DESTRUCTIVE_TESTS=1` and skipped
by default.

## License

MIT. See [`../LICENSE`](../LICENSE) at the repository root.
