# Impreza DevKit

Official client tooling for the [Impreza Host](https://imprezahost.com)
public REST API. Two co-released Python packages plus the OpenAPI 3.1
contract that backs them.

| Package | Install | Docs |
|---|---|---|
| **`impreza-sdk`** | `pip install impreza-sdk` | [`sdk-python/README.md`](sdk-python/README.md) |
| **`impreza-cli`** | `pip install impreza-cli` | [`cli-python/README.md`](cli-python/README.md) |
| OpenAPI 3.1 spec | — | [`openapi/openapi.yaml`](openapi/openapi.yaml) |

Python 3.10+. Linux / macOS / Windows. MIT-licensed.

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

CLI:

```bash
pip install impreza-cli
impreza context create personal --key imp_... --secret ...
impreza doctor               # five-check health verification
impreza vps list
impreza account topup --amount 50 --method xmr --browser --wait
```

See the per-package READMEs above for the full usage walkthroughs.

## Repository layout

```
impreza-devkit/
├── CHANGELOG.md            release history (both packages move in lock-step)
├── LICENSE                 MIT
├── openapi/openapi.yaml    OpenAPI 3.1 contract
├── sdk-python/             impreza-sdk package source + tests
├── cli-python/             impreza-cli package source + tests
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
```

Quality gates: `ruff check` + `mypy --strict impreza` (SDK) /
`mypy --strict impreza_cli` (CLI). The full live-smoke suite runs
against the real API when `IMPREZA_API_KEY` + `IMPREZA_API_SECRET`
are set; without those credentials the suites skip silently and only
the mocked unit tests run.

## Stack

- Python 3.10+
- [`httpx[socks]`](https://www.python-httpx.org/) — sync + async HTTP, SOCKS5 for Tor
- [`pydantic` v2](https://docs.pydantic.dev/) — typed request / response models
- [`typer`](https://typer.tiangolo.com/) + [`rich`](https://rich.readthedocs.io/) — CLI framework + rendering
- [`pytest`](https://docs.pytest.org/) + [`respx`](https://lundberg.github.io/respx/) — unit + HTTP-mock tests
- [`ruff`](https://docs.astral.sh/ruff/) + [`mypy --strict`](https://mypy.readthedocs.io/) — lint + types

## License

[MIT](LICENSE) — © 2026 Impreza Host.
