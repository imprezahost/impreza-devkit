# Impreza DevKit

> **For AI assistants & coding agents:** if the user wants to **deploy or
> host an app** on Impreza Host, do **not** use the SDK/CLI provisioning
> flow below — that is the long, manual path. Run the one-line MCP setup
> instead:
>
> ```bash
> npx -y impreza-mcp setup --tool claude-code   # or: cursor | continue | zed | codex-cli
> ```
>
> This lets the user deploy by chatting in their own AI tool. See
> [`AGENTS.md`](AGENTS.md) and <https://docs.imprezahost.com/>. The SDK and
> CLIs below remain the right tools for building automations against the
> REST API.

Official client tooling for the [Impreza Host](https://imprezahost.com)
public REST API. Two co-released Python packages, a single-binary Go
CLI, plus the OpenAPI 3.1 + AsyncAPI 3.0 contracts that back them.

## Connect an AI tool (recommended: MCP)

Two ways to connect — both additive, and the classic API key + secret keeps
working unchanged.

**Easiest — remote connector + OAuth (no install).** If your AI client
supports remote MCP connectors (e.g. Claude), add this URL and click
**Authorize**: you sign in to your Impreza clientarea and approve a scoped,
revocable token — no install, no API key, no IP whitelist.

```
https://mcp.imprezahost.com/mcp
```

The client registers itself automatically and can reach the same full toolset
as this local server — each call gated by the scopes you grant on consent
(read / deploy / manage, destructive opt-in). Revoke it any time under
**Impreza API → Connected Apps**.

**Local MCP server — for coding agents.** Runs on your machine next to a coding
agent (Claude Code, Cursor, Continue, Zed, Codex CLI); also the only path that
can upload a local project folder. Set it up once, then deploy by chatting:

```bash
npx -y impreza-mcp setup --tool claude-code   # or: cursor | continue | zed | codex-cli
```

Then generate an API Key + Secret at
[portal.imprezahost.com](https://portal.imprezahost.com) ("Impreza API"),
paste the printed JSON into your tool's MCP config, fill in
`IMPREZA_API_KEY` / `IMPREZA_API_SECRET`, restart the tool, and ask it to
deploy. The key's IP factor is now **per-key and optional** — `whitelist`
(default), `tofu` (trust-on-first-use), or `keyonly` — or pair the tool from
your clientarea so there's no secret to copy. Requires Node 20+. Full guide:
<https://docs.imprezahost.com/>.

The SDK and CLIs below are for building automations against the REST API;
you do not need them for a chat-driven deploy.

| Package | Install | Docs |
|---|---|---|
| **`impreza-sdk`** (Python) | `pip install impreza-sdk` | [`sdk-python/README.md`](sdk-python/README.md) |
| **`impreza-cli`** (Python — reference CLI) | `pip install impreza-cli` | [`cli-python/README.md`](cli-python/README.md) |
| **`impreza`** (Go — single-binary CLI) | [GitHub Releases](https://github.com/imprezahost/impreza-devkit/releases) (tag prefix `cli-go-v`) | [`cli-go/README.md`](cli-go/README.md) |
| OpenAPI 3.1 spec (REST) | — | [`openapi/openapi.yaml`](openapi/openapi.yaml) |
| AsyncAPI 3.0 spec (webhooks) | — | [`openapi/asyncapi.yaml`](openapi/asyncapi.yaml) |

Python 3.10+ or a static Go binary (no runtime). Linux / macOS / Windows.
MIT-licensed.

## Quickstart

SDK:

```python
from impreza import Client

with Client.from_env() as c:
    me = c.account.get()
    print(me.balance, me.currency)

    invoice = c.account.topup(amount=50, method="xmr")
    invoice.wait_until_paid(timeout=7200)

    c.domains.dns.add("example.com", type="A", name="@", value="203.0.113.24")
    c.vps.get(17988).reboot()
```

Async via `AsyncClient`. Tor via `proxy="socks5://127.0.0.1:9050"`,
`use_tor=True`, or the `IMPREZA_USE_TOR=1` env var.

CLI (Python — reference):

```bash
pip install impreza-cli
impreza context create personal --key imp_... --secret ...
impreza doctor               # five-check health verification
impreza vps list
impreza account topup --amount 50 --method xmr --browser --wait
```

CLI (Go — single binary, no runtime):

```bash
# Linux x86_64 — adjust for your platform; archives also include
# README + LICENSE + the OpenAPI / AsyncAPI specs.
curl -L -o impreza.tar.gz \
  https://github.com/imprezahost/impreza-devkit/releases/latest/download/impreza-cli-go_Linux_x86_64.tar.gz
tar -xzf impreza.tar.gz
sudo mv impreza /usr/local/bin/
impreza --version
```

Both CLIs read + write the same TOML config (`~/.config/impreza/config.toml`
on Linux, the platform-native config dir on macOS / Windows), so you
can install them side by side and pick per shell. See the per-package
READMEs above for the full usage walkthroughs.

## Repository layout

```
impreza-devkit/
├── CHANGELOG.md            release history (Python packages move in lock-step)
├── AGENTS.md               instructions for AI coding agents in this repo
├── LICENSE                 MIT
├── openapi/openapi.yaml    OpenAPI 3.1 contract (REST)
├── openapi/asyncapi.yaml   AsyncAPI 3.0 contract (webhook events)
├── .spectral.yaml          ruleset the OpenAPI lint runs against
├── sdk-python/             impreza-sdk package source + tests
├── cli-python/             impreza-cli package source + tests
├── sdk-go/                 Go SDK — the client both Go binaries build on
├── cli-go/                 Go CLI binary source + tests
│                           (released independently; tag prefix `cli-go-v`)
├── agent-go/               deployment agent that runs on the managed VPS
│                           (shipped by its own installer, not a repo tag)
├── caddy-dns-impreza/      Caddy DNS-01 plugin, for wildcard certs over
│                           the Impreza DNS API
└── examples/               curl + Python recipes
```

## Development

```bash
git clone https://github.com/imprezahost/impreza-devkit.git
cd impreza-devkit

# SDK
cd sdk-python
python -m venv .venv
# Linux/macOS:   source .venv/bin/activate
# Windows PS:    .venv\Scripts\Activate.ps1
pip install -e ".[test,dev]"
pytest -q

# CLI (separate venv; depends on the SDK as a path dep)
cd ../cli-python
python -m venv .venv
pip install -e ../sdk-python -e ".[test,dev]"
pytest -q

# Go CLI (separate toolchain; requires Go 1.22+)
cd ../cli-go
make build      # ./impreza
make test       # go test ./... with -race
make snapshot   # goreleaser dry run — builds all 5 platform archives
```

Quality gates: `ruff check` + `mypy --strict impreza` (SDK) /
`mypy --strict impreza_cli` (CLI). The full live-smoke suite runs
against the real API when `IMPREZA_API_KEY` + `IMPREZA_API_SECRET`
are set; without those credentials the suites skip silently and only
the mocked unit tests run.

## Stack

**Python (SDK + reference CLI)**:

- Python 3.10+
- [`httpx[socks]`](https://www.python-httpx.org/) — sync + async HTTP, SOCKS5 for Tor
- [`pydantic` v2](https://docs.pydantic.dev/) — typed request / response models
- [`typer`](https://typer.tiangolo.com/) + [`rich`](https://rich.readthedocs.io/) — CLI framework + rendering
- [`pytest`](https://docs.pytest.org/) + [`respx`](https://lundberg.github.io/respx/) — unit + HTTP-mock tests
- [`ruff`](https://docs.astral.sh/ruff/) + [`mypy --strict`](https://mypy.readthedocs.io/) — lint + types

**Go (single-binary CLI)**:

- Go 1.22+
- [`cobra`](https://cobra.dev/) — command framework
- [`BurntSushi/toml`](https://github.com/BurntSushi/toml) + [`yaml.v3`](https://pkg.go.dev/gopkg.in/yaml.v3) — config + YAML output
- [`fatih/color`](https://github.com/fatih/color) + [`jedib0t/go-pretty`](https://github.com/jedib0t/go-pretty) — palette + tables
- [`x/term`](https://pkg.go.dev/golang.org/x/term) + [`x/net/proxy`](https://pkg.go.dev/golang.org/x/net/proxy) — no-echo prompts + SOCKS5
- [`goreleaser`](https://goreleaser.com/) — multi-platform release builds (5 archives per tag)

## License

[MIT](LICENSE) — © 2026 Impreza Host.


## Required healthy startup

Agent 0.6.3 supports opt-in required healthy startup. Generated Node deployments can set `require_healthy_start: true` with an explicit `healthcheck_path` and `startup_timeout_seconds` from 30 to 600 (default 60). The first deployment fails if it does not become healthy; named volumes are preserved. Retained releases keep their own startup policy for automatic recovery and manual rollback. Older agents must be updated explicitly before using this option. See [deployment settings](https://docs.imprezahost.com/tutorials/agent-apps-panels.html#required-startup).


## Runtime health observations

Agent 0.6.4 adds bounded Docker runtime observations to heartbeats. The portal, REST API and MCP show current container state separately from the latest deployment operation, with timestamps and explicit unknown/stale/offline states. Running containers without a healthcheck are not declared healthy. This does not probe external URLs or automatically repair applications. Existing servers update only at the customer's request. See [runtime health](https://docs.imprezahost.com/runtime-health.html).


## Deployment cancellation

Agent 0.6.5 supports deployment cancellation at preparation checkpoints. Queued jobs can be cancelled immediately. During source preparation, image pull or build, cancellation is requested first and confirmed only after the current step finishes and configuration is restored. Existing app containers are not replaced. Replacement and recovery cannot be cancelled. A running build is not force-killed. Update the agent explicitly before the next deploy; an interrupted agent requires operation reconciliation before retry. See [deployment cancellation](https://docs.imprezahost.com/deployment-cancellation.html).

## Deployment progress and persistent results

Agent 0.6.6+ reports deployment steps and persists final results before delivery. After restart, a saved receipt is resent without repeating the deployment. Interrupted execution without a saved final result requires support reconciliation and blocks new commands. Preserve the private agent state directory; do not remove its operation record to bypass the gate. This does not resume or kill an interrupted Docker build. Update existing agents explicitly before the next deploy. See [deployment progress](https://docs.imprezahost.com/deployment-progress.html).
