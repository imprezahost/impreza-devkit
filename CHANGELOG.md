# Changelog

All notable changes to `impreza-sdk` and `impreza-cli` are recorded
here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once we leave alpha — pre-1.0 we treat every minor as potentially
breaking.

This file is the canonical user-facing history for both packages.
Both ship in lock-step — every release tags `sdk-v<version>` and
`cli-v<version>` on the same commit.

## [Unreleased]

### Added

- **In-place redeploy for custom deployments.** Rebuild a running custom
  app from its CURRENT source — re-pull the image, re-clone the watched
  git ref at its new HEAD, or rebuild — without uninstalling first. The
  deployment id, domain, and host port are all preserved, so the app's
  URL never changes. This is the in-place alternative to uninstall +
  recreate for shipping a new build.
  - `impreza-cli` (Go): `impreza platform deployments redeploy <id>` —
    `--env KEY=VALUE` (repeatable) merges env before the rebuild;
    `--follow` blocks until it settles.
  - `impreza-sdk` (Go): `Client.PlatformRedeployCustomDeployment(ctx, id, req)`.

### Changed

- Custom deployments now keep a **stable domain**. Recreating a custom
  deployment under a name it previously used reuses that app's
  auto-allocated `*.imprezaapps.com` subdomain instead of minting a new
  one, so neither an in-place redeploy nor an uninstall + recreate
  changes your URL.

### Fixed

- Redeploying a build-mode (Dockerfile / git) custom deployment now rebuilds
  the image on every run, so `impreza platform deployments redeploy` always
  ships the latest commit. Requires the server agent at **0.5.1** or newer
  (re-run the install script on the VPS to update).

## [0.4.0] — 2026-05-19

Adds the `dedicated` resource — a new top-level namespace mirroring
the public `/dedicated/*` API surface for dedicated-server management.
Operations are gated by per-service capabilities advertised through
`GET /dedicated/{id}/capabilities`; calling a capability-gated
endpoint against a service that doesn't advertise it returns
`NOT_SUPPORTED`. Always inspect capabilities first when scripting.

The dedicated surface is vendor-agnostic — the public payload never
exposes the underlying backend identity, so the same code works
across every dedicated service on the account.

### Added

- **`impreza-sdk`: `client.dedicated` / `async_client.dedicated`**
  resource (`impreza/resources/dedicated.py`). 17 methods per side
  covering the full `/dedicated/*` surface — list, info, capabilities,
  status, ips, os-images, kvm, firewall, firewall/logs, bandwidth,
  vpn, start, shutdown, reboot, set_rdns, reset_rdns, reinstall,
  enable_kvm, disable_kvm, set_firewall.
  - `reinstall(...)` is destructive: the SDK rejects `confirm=False`
    locally and injects the required `X-Impreza-Confirm: WIPE`
    header alongside the body confirmation.
  - `_http` and `_http_async` gained an optional `headers` kwarg on
    `post` so out-of-band confirmation headers travel through the
    same retry / envelope handling as every other call.
- **`impreza-cli` (Python): `impreza dedicated` command tree**
  (`impreza_cli/commands/dedicated.py`). 20 sub-commands matching
  the SDK methods, including the destructive `reinstall` verb gated
  behind both `--confirm` and a TTY prompt (or `--yes`).
- **`impreza-cli-go` (Go): `impreza dedicated` command tree**
  (`cli-go/cmd/dedicated.go` + `cli-go/internal/client/dedicated.go`).
  Same 20 verb surface as the Python CLI. `PostWithHeaders` /
  `doWithHeaders` added to the internal HTTP client so the reinstall
  verb can inject `X-Impreza-Confirm: WIPE` without bypassing the
  shared retry / envelope handling.

### Notes

- The verb surface is **lock-step across both CLIs** (Python and Go)
  and the SDK — 20 dedicated verbs on each, mapping 1:1 to the
  server's `/dedicated/*` endpoints. Cosmetic differences only
  (option naming, output renderers).
- Total verb counts after this release: **106 verbs** on both
  `impreza-cli` (Python) and `impreza-cli-go` (Go), across 11
  resource groups.
- This release is **purely additive**. No existing surface changes;
  no breaking changes. Upgrade with `pip install -U impreza-sdk
  impreza-cli` (Python) or downloading the new `cli-go-v0.2.0`
  binaries (Go).

## [0.3.2] — 2026-05-11

Hot-fix for `0.3.1`. Removes the rDNS sub-surface from the
Python CLI (it never worked end-to-end against the live API), and
folds in the docs + Go-CLI bootstrap that had been sitting in
Unreleased since AsyncAPI shipped.

### Removed

- **`impreza vps cloud rdns get / set / delete`** — the three
  rDNS verbs are gone from the CLI surface. The public-edge
  WAF rejects `/vps/cloud/rdns/{ip}` paths with dotted-IPv4
  segments and returns an HTML maintenance page instead of
  JSON, so the verbs were unusable end-to-end. Library-level
  access via the SDK
  (`client.vps.get(id).rdns.get / set / delete`) is unchanged
  — the WAF is a public-edge concern that library consumers
  can route around (custom transport, alternate endpoint,
  etc.). The verbs will return to the CLI surface once the
  server-side WAF rule is fixed. See git history at tag
  `cli-v0.3.1` for the prior CLI command shape if you want
  to recreate equivalent calls via SDK directly.

### Added (docs + parallel surfaces — landed since 0.3.1)

- **`openapi/asyncapi.yaml`** — AsyncAPI 3.0 spec documenting
  the webhook delivery contract. Covers the 16 event types
  (`webhook.test`, `topup.paid`, `invoice.created/paid`,
  `order.created`, `service.activated/suspended/cancelled`,
  `domain.registered/transferred/expiring_soon/expired`,
  `vps.power_state_changed/backup_completed/snapshot_created/reinstall_completed`),
  the 3 wildcard patterns (`*`, `vps.*`, `domain.*`), the
  `EventEnvelope` shape, per-event payload schemas, the
  `X-Impreza-Signature` HMAC-SHA256 security scheme, and the
  standard delivery headers. Cross-linked from `openapi.yaml`,
  both package READMEs, and the root README.
- **Event firing status note:** only `webhook.test` is actively
  fired (delivery probe on subscription create). The other 15
  event types are documented as the public contract; the
  server-side hooks that fire them are being wired
  progressively. The wire shape is stable — subscribers can
  register for any event today; they'll start receiving them
  as the hooks land server-side.
- **`cli-go/` — Go single-binary CLI (`impreza`)**, shipping
  in parallel as `cli-go-v0.1.0` (and `cli-go-v0.1.1` for the
  rDNS removal). Releases ship on the independent
  `cli-go-v<version>` tag track (separate from the Python
  lock-step `sdk-v` / `cli-v` cycle) and upload to GitHub
  Releases as five static-binary archives (Linux x86_64+arm64,
  macOS x86_64+arm64, Windows x86_64), each bundling the
  binary plus `README.md`, `LICENSE`, `openapi.yaml`, and
  `asyncapi.yaml`. The Python CLI remains the reference
  implementation; both CLIs share the same TOML config and
  verb surface (86 verbs across 10 resource groups after the
  rDNS removal), so users can install them side by side.

### Notes

- **SDK has zero code changes** in 0.3.2. The version bump is
  for lock-step consistency with `impreza-cli` 0.3.2; both
  packages always release at the same tag.
- The 86-verb count applies to BOTH `impreza-cli` (Python)
  and `impreza-cli-go` (Go). They removed rDNS at the same
  release — Python in `cli-v0.3.2`, Go in `cli-go-v0.1.1`.

## [0.3.1] — 2026-05-11

Hot-fix for `0.3.0`. Both packages shipped `__version__ = "0.1.0a0"`
hard-coded in their `__init__.py` from the Phase-1 release —
`pip show impreza-sdk` correctly reported `0.3.0` (read from
wheel metadata) but `impreza.__version__` and `impreza --version`
still claimed `0.1.0a0`.

### Fixed

- **`impreza.__version__`** (SDK) + **`impreza_cli.__version__`**
  (CLI) now read the live package metadata via
  `importlib.metadata.version()`. They always match the installed
  wheel; no manual bump needed at release time. Falls back to
  `"0.0.0+unknown"` for source checkouts without an install
  (e.g. `python -c "from impreza import __version__"` run
  directly out of the git tree).

No other changes — same code surface as `0.3.0`.

## [0.3.0] — 2026-05-11

First general-availability release after a successful private-beta
window. Tagged simultaneously as `sdk-v0.3.0` and `cli-v0.3.0`;
also the first PyPI publish for both packages (`pip install
impreza-sdk` and `pip install impreza-cli` work without any token
from this release forward).

This is the same code as `0.2.0a0`, with sensitive-content +
vendor-name scrubs applied for the public-mirror flip. No
breaking API changes, no new verbs — purely the release cut +
polish needed to ship publicly.

### Polish since 0.2.0a0

- **Customer-facing terminology**: surfaced platform name as
  "Impreza Account" everywhere it was previously named after the
  underlying billing system (117 occurrences across CLI help
  text, error messages, README copy, OpenAPI doc, SDK docstrings,
  and test fixtures).
- **Vendor naming neutralised**: the upstream Cloud VPS provider's
  brand was named in customer-facing strings (36 occurrences
  across `vps_cloud.py` help text, SDK docstrings, etc.).
  Renamed to generic `Cloud` / `Cloud backend` consistently.
- **Support email standardised** to `support@imprezahost.com`
  in `pyproject.toml` author metadata and feedback channels.
- **Public-facing docs rewritten**: root `README.md` reduced from
  151 to ~90 lines, removed phase-by-phase status table and
  private-repo links. Per-package READMEs point at the public
  GitHub repo for clone URLs.

### Quality gates at release

- SDK: **266 passed + 29 skipped**, `mypy --strict` clean (37
  files), `ruff check` clean.
- CLI: **307 passed + 41 skipped**, `mypy --strict` clean (21
  files), `ruff check` clean.

### Installation

```bash
pip install impreza-sdk
pip install impreza-cli
```

Python 3.10+. Linux, macOS, Windows.

## [0.2.0a0] — 2026-05-11

Second alpha cut. Covers all of Phase 3 (CLI write surface + sub-
resources) and Phase 4 (Async parity + UX polish + release prep).
Tagged simultaneously as `sdk-v0.2.0-alpha` and `cli-v0.2.0-alpha`
because the two packages move in lock-step at this stage.

### SDK — Added

- **`c.account.api_key_self()`** — wraps `GET /account/api-keys/self`.
  Returns the calling key's prefix, label, status, IP whitelist,
  rate limit, and the `request_ip` the server observed. Used by
  the new CLI `impreza doctor` command for the IP-whitelist health
  check.
- **`c.account.services.cancel(service_id, *, type, reason=None)`**
  + async equivalent. POSTs `/services/{id}/cancel`. Client-side
  validates `type` against `{"Immediate", "End of Billing Period"}`
  and raises `ValueError` for anything else before any HTTP. Returns
  `None` on success — the server emits an `AddCancelRequest` for
  staff approval. (Service termination is staff-owned: customers
  open a request, the team approves the actual termination.)

### SDK — Changed

- `Domain` model now tolerates server responses where the `domain`
  field is omitted (some upstreams elide it on certain status
  values). Caught by a live smoke; the model gained a default
  fallback so existing call sites don't break.
- README "Async" block expanded with:
  - "Reach for AsyncClient when you need high-fanout calls"
    framing.
  - Examples of awaiting `AsyncOperation` (snapshot rollback) and
    `AsyncTopupInvoice` (crypto payment wait).
  - Explicit paragraph documenting that the CLI is sync by design;
    library users do async via `AsyncClient` directly.

### SDK — Removed

- **`Vps.suspend()` / `Vps.unsuspend()`** (sync + async). The
  server-side endpoints `/vps/proxmox/{id}/suspend` and
  `/.../unsuspend` were retired the same day because service
  suspension is a billing-state operation owned by staff workflows.
  Customer-facing path to pause a guest is now `vps shutdown`; for
  service wind-down, `vps cancel` (which submits an
  `AddCancelRequest`).

### SDK — Tested

- **6 new async parity unit tests** filling per-resource gaps:
  catalog `product` + `product_groups`, invoices status-filter
  list, webhooks `get` + `update` + `deliveries`. Pre-4.4a floor
  was 2 async tests per resource (catalog / invoices); after, 3.
- SDK test totals at 0.2.0a0: **266 passed + 29 skipped** (up from
  254/29 at 0.1.0a0).

### CLI — Added

This is the package's first proper release — `0.1.0a0` only
shipped the SDK. The CLI's 0.2.0a0 corresponds to roughly 65
shipping verbs across 11 command groups, organised below by
resource. Each verb maps to a single SDK method or a
mutation-result render.

**Read commands (carried over from Phase 2):**

- `impreza account info / balance / services [--status STATUS]`
- `impreza catalog products [--group G] / product <id> /
  product-groups / tlds [--filter .com,.net]`
- `impreza context create / use / list / current / delete`
- `impreza domain show / check / pricing` + `impreza domain dns list`
- `impreza invoice list [--status] / show <id>`
- `impreza key whoami` — wraps `c.account.api_key_self()`.
- `impreza vps list [--backend] [--status] / show <id> / status <id>`

**Write commands (Phase 3.1 → 3.7, 64 new verbs):**

- **Domains (3.1):** `register / transfer / set-nameservers /
  lock / unlock / id-protection / raa-verify / gdpr-auth /
  transfer-approval` + `domain dns add / update / delete /
  activate`. Cost-incurring verbs (`register`, `transfer`,
  `id-protection`) gated by `confirm_or_exit`.
- **VPS power (3.2):** `vps start / stop / reboot / shutdown`.
  `stop` is force-poweroff (corruption risk) and gated by
  `confirm_or_exit`; the other three skip confirmation.
- **VPS management (3.3):** `vps set-hostname / set-password /
  reinstall / migrate / cancel`. `reinstall` and `migrate`
  return Operation futures and expose `--wait` (default
  timeouts 600s and 1800s respectively). `set-password` and
  `reinstall --password` use Typer's hidden-prompt +
  confirmation_prompt pattern. `vps cancel` defaults `--type
  "End of Billing Period"` to protect prepaid days.
- **Proxmox sub-resources (3.4):** `vps proxmox snapshots
  list/create/delete/rollback` + `vps proxmox backups
  list/create/restore/delete` + `vps proxmox backup-schedules
  list/create/delete` + `vps proxmox network reconfigure`.
  Rollback / backup-create / backup-restore wrap Operation
  futures with `--wait`.
- **Cloud sub-resources (3.5):** `vps cloud images
  list/create/restore/delete` + `vps cloud rescue
  enable/disable` + `vps cloud iso mount/unmount` + `vps
  cloud rdns get/set/delete` + `vps cloud ssh-keys list/assign`
  + inline `vps cloud vnc / vnc-password / resize / boot-order
  / ipv6 enable`. `boot-order` validates `--order` against
  `{cda, dca}` client-side.
- **Orders + crypto top-up (3.6):** `order list / show /
  create / upgrade` + `account topup --amount [--method]
  [--browser] [--wait] [--timeout]` + `account topup-status
  <invoice-id>`. `order create` accepts both ID-keyed and
  name-keyed `--config-option` / `--custom-field` flags.
  `account topup --browser` opens `payment_url` in the system
  browser; `--wait` polls with elapsed + ETA-until-expiry
  rendered in place via carriage-return redraw.
- **Service cancel + webhooks CRUD (3.7):** `service cancel
  <id>` (non-VPS equivalent of `vps cancel`); `webhook list /
  show / create / update / delete / rotate-secret / deliveries
  / event-types`. `create` and `rotate-secret` print the HMAC
  secret "shown only once" with a store-it warning.

**Phase 4 polish + utilities:**

- **`impreza doctor`** (4.1) — first-line support / health
  check. Runs five sequenced checks (active context, API
  reachable, key status, IP whitelist match, account profile),
  renders `[OK] / [FAIL] / [WARN] / [SKIP]` ASCII labels in
  coloured output, supports `--output json` for monitoring
  scripts. Exit 0 only if every check passed.
- **Crypto top-up UX polish (4.2):** `--browser` flag on
  `account topup` to auto-open `payment_url`; `--wait` poll
  now redraws a single in-place line showing elapsed seconds
  + ETA-until-invoice-expiry parsed from the server's
  `expires_at`.
- **Cross-cutting palette (4.3 + 4.4b):** new `success()` /
  `info()` / `warning()` helpers in `impreza_cli.output`
  alongside the existing `error()`. Adopted across 28
  mutation-confirmation lines in 9 command modules — green
  for "X created/deleted/updated", cyan for "queued / reboot
  to apply", red for errors, yellow for warnings.
- **Multi-context, multi-output (carried from Phase 2):**
  `--context NAME` global override, `--output table|json|yaml`
  for every read verb. Tab completion via `impreza
  --install-completion`.

### CLI — Tested

- **307 unit tests + 41 live smokes** at 0.2.0a0 (up from
  ~165 at start of Phase 3). 41 live smokes skip cleanly when
  `IMPREZA_API_KEY` / `IMPREZA_API_SECRET` are not in the
  environment — CI stays runnable without secrets.
- With creds + `IMPREZA_TEST_VPS_ID` + `IMPREZA_TEST_CLOUD_VPS_ID`
  + `IMPREZA_TEST_DOMAIN` + `IMPREZA_DESTRUCTIVE_TESTS=1`,
  the full suite passes **319 of 326** tests at the Phase 3
  close (the 7 skips are `IMPREZA_TEST_DOMAIN`-gated 3.1
  destructive smokes when no test domain is set, the
  `IMPREZA_TEST_ALLOW_PASSWORD_RESET`-gated 3.3 set-password
  smoke, and the 3.5 rdns smoke skipped on the open WAF /
  mod_security follow-up — see plan doc).

### Server-side changes (companion deploys)

These shipped on the server side and reached production
during Phase 3 / 4 — listed here so the SDK / CLI release notes
form a complete picture:

- `402168a` Detect upstream "Failed" envelope on DNS endpoints
  (3.1).
- `915a70c` Retire customer-facing `suspend` / `unsuspend` on
  Proxmox VPS (post-3.3 policy correction).
- `d59de1c` Normalize Proxmox snapshot create response to a
  Snapshot object — was leaking a Proxmox UPID string (3.4).
- `1ab772a` Normalize Cloud VNC response to `{ip, port,
  password}` — was leaking the raw Cloud operator
  payload (3.5).

### Open follow-up (carried forward)

- `GET /vps/cloud/rdns/{dotted-ipv4}` returns an HTML
  maintenance/CAPTCHA page from the application firewall —
  reproducible via direct httpx call. Three fix options
  documented in the plan doc's "Open follow-up" section. Not
  blocking the 0.2.0a0 release but worth fixing before
  customer-facing ramp.

## [0.1.0a0] — 2026-05-09

First alpha cut. Feature-complete for the SDK surface that ships in
Phase 1. CLI lives in a sibling package and lands in Phase 2.

### Added

- **Sync `Client` and async `AsyncClient`** with shared transport
  (`httpx`), auth-header injection (`X-API-Key` + `X-API-Secret`),
  exponential-backoff retry on 5xx and 429 (respecting `Retry-After`),
  envelope-aware response unwrapping, and typed exception mapping
  off both HTTP status and the API's `error.code`.
- **Tor routing** out of the box. `proxy="socks5://127.0.0.1:9050"`
  for explicit, `use_tor=True` / `IMPREZA_USE_TOR=1` for opt-in,
  `auto_tor=True` for "probe Tor and fall back to clearnet". Backed
  by `httpx[socks]`; sync and async parity.
- **Resources (sync and async, all live-smoked against
  api.imprezahost.com):**
  - `c.account` — profile, balance, services list, single-service
    detail (3 ops), plus crypto top-up (see below).
  - `c.catalog` — `products`, `product`, `product_groups`, `tlds`
    (4 ops). Verb-style methods because catalog is reference data,
    not a managed collection.
  - `c.invoices` — `list`, `get` (2 ops; pay-from-balance lives on
    `c.invoices.pay` and is wired but not destructively-smoked).
  - `c.domains` — 13 top-level methods (check, get, register,
    transfer, set_nameservers, lock/unlock, activate_dns,
    purchase_id_protection, resend_raa_verification,
    resend_gdpr_auth, resend_transfer_approval) plus nested
    `c.domains.dns` with full CRUD (list, add, update, delete) —
    16 operations over 13 paths.
  - `c.hosting` — `get`, `nameservers`, `trigger_autossl` (3 ops).
    Most cPanel summary fields forwarded as `dict[str, object]`
    because the upstream shape varies enough across plans that a
    tight model would be brittle.
  - `c.email.titan` — `get`, `dns_records`, `sso` (3 ops).
    `TitanSsoUrl` is the only typed model; rest forwarded.
  - `c.email.google` — `get`, `dns_records` (account-scoped),
    `setup_admin` (3 ops).
  - `c.orders` — `list` (50 most recent, optional status filter),
    `get`, `create`, `upgrade` (4 ops). `create` / `upgrade` accept
    both ID-keyed and **name-keyed** dicts for `config_options` and
    `custom_fields`; resolution is automatic via one extra
    `GET /products/{id}` lookup. Resolution failures raise
    `InvalidRequest(UNKNOWN_OPTION / UNKNOWN_FIELD)` *before* any
    `/orders` call — no half-failed orders.
  - `c.vps` — **smart-dispatch** (decision §11.I, Option B): one
    `c.vps.get(service_id)` returns a backend-aware `Vps` bound model
    that routes Proxmox vs Cloud operations
    transparently. Common surface: `start` / `stop` / `reboot` /
    `shutdown` / `set_hostname` / `set_password` / `reinstall` /
    `status` / `refresh` / `cancel`. Backend-specific sub-resources
    expose the rest: `vps.snapshots` / `vps.backups` /
    `vps.backup_schedules` / `vps.operations` / `vps.console` /
    `vps.console_ssh` / `vps.config` / `vps.pending` /
    `vps.resources` / `vps.ips` / `vps.available_ips` /
    `vps.templates` / `vps.locations` / `vps.network_reconfigure` /
    `vps.migrate` / `vps.suspend` / `vps.unsuspend` (Proxmox-only);
    `vps.images` / `vps.rescue` / `vps.iso` / `vps.rdns` /
    `vps.ssh_keys` / `vps.vnc` / `vps.vnc_password` / `vps.resize`
    / `vps.boot_order` / `vps.ipv6_enable` (Cloud-only). Wrong-
    backend access raises `BackendNotSupported(backend, operation,
    hint=...)` — a client-side guard, not a network call.
  - `c.webhooks` — subscription CRUD (`list` / `get` / `create` /
    `update` / `delete` / `rotate_secret` / `deliveries` /
    `event_types`). Local guards: empty `events=[]` on create raises
    `ValueError`; empty `update()` raises `ValueError` — both before
    any HTTP call.
- **Action polling.** Long-running Proxmox operations
  (`snapshots.rollback`, `backups.create` / `backups.restore`,
  `vps.migrate`, `vps.reinstall`) return an `Operation` (sync) /
  `AsyncOperation` (async) future with `.wait(timeout=, poll_interval=)`,
  `.refresh()`, and status predicates (`is_done`, `is_success`,
  `is_failure`). Status normalization across upstream variants
  (`completed` / `complete` / `success` / `succeeded` / `done` for
  success; `failed` / `cancelled` / `canceled` / `error` for
  failure). `OperationTimeout` / `OperationFailed` exceptions on
  bad terminal states.
- **Crypto top-up.** `c.account.topup(amount=, method=)` returns a
  `TopupInvoice` future routed through the existing `btcpayinline`
  gateway (BTC, XMR, TRX, USDT-TRC20). `wait_until_paid(timeout=,
  poll_interval=)` polls `GET /account/topup/{id}` until terminal
  (`paid` → success; `cancelled` / `refunded` / `expired` →
  `TopupFailed`). Defaults: 30s poll interval, 7200s timeout
  (matches server-side invoice expiry). `payment_url` and
  `expires_at` from the create response carry forward across
  `refresh()` even though the status endpoint doesn't echo them.
- **Webhook receiver helpers** in `impreza.webhooks`. Top-level
  module *not* a resource because applications **receiving**
  webhooks don't need an API client at all:
  - `verify_signature(body=, signature_header=, secret=)` →
    `WebhookEvent`. Timing-safe HMAC-SHA256 compare via
    `hmac.compare_digest`. Raises `WebhookSignatureMismatch` with a
    deliberately vague message ("signature mismatch") — leaking
    which half of the comparison failed first would let attackers
    narrow signatures byte-by-byte.
  - `parse_event(body)` — JSON parse + Pydantic validation only
    (no signature check). For "you already verified elsewhere" or
    local fixtures.
  - `compute_signature(body, secret)` — receiver-side fixture
    helper.
- **Pagination utilities.** `iter_all()` for endpoints that page
  via `meta.pagination`. Foundation laid in 1.1; current API surface
  doesn't actually page yet (`/invoices` returns up to 100), but
  the helper is ready when the server starts emitting pagination
  metadata.
- **Typed exception hierarchy.** Every error is a subclass of
  `ImprezaError`, so `except ImprezaError` catches anything the
  SDK can throw. Specific subclasses: `NetworkError`, `ApiError`
  (and its status-code-specific subclasses `AuthError`,
  `PermissionDenied`, `IpNotWhitelisted`, `ResourceNotFound`,
  `InvalidRequest`, `InsufficientCredit`, `RateLimitExceeded`,
  `UpstreamError`, `ServerError`), `OperationTimeout`,
  `OperationFailed`, `TopupTimeout`, `TopupFailed`,
  `WebhookSignatureMismatch`, `BackendNotSupported`.

### Stack

- Python 3.10+
- `httpx[socks]` ≥ 0.27 — sync + async, SOCKS5 for Tor
- `pydantic` ≥ 2.5 — typed response models with runtime validation
- `pytest` + `respx` — HTTP mocking in tests
- `ruff` + `mypy --strict` — lint and type-check

### Tested

- **254 unit tests + 29 live smoke tests.** Smokes skip silently
  when `IMPREZA_API_KEY` / `IMPREZA_API_SECRET` are not set, so the
  suite stays runnable on CI runners without secrets.
- Final live run (2026-05-09 against api.imprezahost.com,
  4 VPS services across 2 backends): **278 passed, 5 skipped, 0
  failed.** The 5 skips are gated mutating tests
  (`IMPREZA_DESTRUCTIVE_TESTS=1`) and read-only smokes that
  silently skip when the test account lacks the relevant service
  (e.g. Titan email).

### Phase 1 sub-deliverable history (for context)

| Sub | Merged | Scope |
|---|---|---|
| 1.1 | 2026-05-08 | Sync `Client`, auth, retry/backoff, errors, pagination |
| 1.2 | 2026-05-08 | `AsyncClient`, Tor (proxy + IMPREZA_USE_TOR + auto_tor) |
| 1.3 | 2026-05-08 | Read-only resources: `account.services`, `catalog`, `invoices` |
| 1.4a | 2026-05-08 | `domains` (13 ops) + nested `domains.dns` (4 ops) |
| 1.4b-i | 2026-05-08 | Common VPS ops via smart dispatch (bound model) |
| 1.4b-ii | 2026-05-09 | Backend-specific VPS sub-resources |
| 1.4c | 2026-05-09 | `hosting`, `email.titan`, `email.google` |
| 1.4d | 2026-05-09 | `orders` with smart name/id resolution |
| 1.5 | 2026-05-09 | Action polling (`Operation` futures) |
| 1.6 | 2026-05-09 | Webhooks resource + receiver helpers |
| 1.7 | 2026-05-09 | Crypto top-up (`TopupInvoice` future) |
| 1.8 | 2026-05-09 | Release prep — this CHANGELOG, README polish, tag |

[Unreleased]: https://github.com/imprezahost/impreza-devkit/compare/sdk-v0.4.0...master
[0.4.0]: https://github.com/imprezahost/impreza-devkit/compare/sdk-v0.3.2...sdk-v0.4.0
[0.3.2]: https://github.com/imprezahost/impreza-devkit/compare/sdk-v0.3.1...sdk-v0.3.2
[0.3.1]: https://github.com/imprezahost/impreza-devkit/compare/sdk-v0.3.0...sdk-v0.3.1
[0.3.0]: https://github.com/imprezahost/impreza-devkit/compare/sdk-v0.2.0-alpha...sdk-v0.3.0
[0.2.0a0]: https://github.com/imprezahost/impreza-devkit/compare/sdk-v0.1.0-alpha...sdk-v0.2.0-alpha
[0.1.0a0]: https://github.com/imprezahost/impreza-devkit/releases/tag/sdk-v0.1.0-alpha
