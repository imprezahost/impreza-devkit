# Impreza DevKit

Official client tooling for the [Impreza Host](https://imprezahost.com)
public REST API. Two co-released Python packages, a single-binary Go
CLI, plus the OpenAPI 3.1 + AsyncAPI 3.0 contracts that back them.

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

    c.domains.dns.add("example.com", type="A", name="@", value="1.2.3.4")
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
├── LICENSE                 MIT
├── openapi/openapi.yaml    OpenAPI 3.1 contract (REST)
├── openapi/asyncapi.yaml   AsyncAPI 3.0 contract (webhook events)
├── sdk-python/             impreza-sdk package source + tests
├── cli-python/             impreza-cli package source + tests
├── cli-go/                 impreza-cli-go binary source + tests
│                           (released independently; tag prefix `cli-go-v`)
├── caddy-dns-impreza/      libdns provider that proxies ACME DNS-01
│                           challenges through the Impreza public API
│                           — bundled into the Caddy sidecar image used
│                           by impreza-agent for HTTPS termination
└── examples/               curl + Python recipes
```

The Caddy sidecar image (`ghcr.io/imprezahost/caddy:2-cf`) is built +
pushed by `.github/workflows/caddy-image.yml` from
`agent-go/packaging/caddy/Dockerfile` + `caddy-dns-impreza/`. Triggers
on `caddy-v<X.Y.Z>` tag push or manual workflow dispatch.

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
