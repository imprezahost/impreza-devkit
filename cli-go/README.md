# `impreza-cli` (Go) — Single-binary CLI for the Impreza Host API

Go port of the Python `impreza-cli`, distributed as a statically-linked
single binary. Same surface, same multi-context config, same
table/JSON/YAML output, same Tor support. **Pre-release** — Phase 7
of the [DevKit roadmap](../README.md); ships in fases 7.1 → 7.6.

All 86 commands across 10 resource groups (`account`, `catalog`,
`context`, `doctor`, `domain` + `dns`, `invoice`, `key`, `order`,
`service`, `vps` + `proxmox/cloud` sub-namespaces, `webhook`).
Full verb-surface parity with the Python CLI — every verb the
Python `impreza-cli` ships is also in this binary, same flags,
same TOML config, same output formats.

> **rDNS verbs deactivated.** `vps cloud rdns get / set / delete`
> were in 0.1.0 but are removed from both CLIs in 0.1.1 — the
> public-edge WAF rejects `/vps/cloud/rdns/{ip}` paths with
> dotted-IPv4 segments and returns an HTML maintenance page
> instead of JSON. The verbs will return when the WAF rule is
> fixed server-side. The underlying SDK methods stay available
> (`client.vps.get(id).rdns.get/set/delete` in Python,
> `c.CloudRdnsGet/Set/Delete` in Go).

The Python CLI (`pip install impreza-cli`) is the **reference
implementation** and stays the production-recommended path for now.
The Go CLI exists for infra teams that want one binary with no
runtime dependency (the model Hetzner / DigitalOcean / Vultr CLIs
all follow).

## Install

### Pre-built binaries (recommended)

Download the archive for your OS + architecture from the [latest
GitHub Release](https://github.com/imprezahost/impreza-devkit/releases),
extract, and put `impreza` somewhere on your `$PATH`.

```bash
# Linux (x86_64) — adjust for your version + arch
curl -L -o impreza.tar.gz \
  https://github.com/imprezahost/impreza-devkit/releases/download/cli-go-v0.1.0/impreza-cli-go_0.1.0_Linux_x86_64.tar.gz
tar -xzf impreza.tar.gz
sudo mv impreza /usr/local/bin/
impreza --version
```

Available platforms:
- Linux: x86_64, arm64 (`.tar.gz`)
- macOS: x86_64 (Intel), arm64 (Apple Silicon) (`.tar.gz`)
- Windows: x86_64 (`.zip`)

Each archive bundles the binary, `README.md`, `LICENSE`, and the
OpenAPI 3.1 + AsyncAPI 3.0 contract specs.

### From source

Requires Go 1.22+ (tested on 1.26):

```bash
git clone https://github.com/imprezahost/impreza-devkit.git
cd impreza-devkit/cli-go
make build         # ./impreza
./impreza --version
```

## Build

Requires Go 1.22+ (tested on 1.26):

```bash
cd cli-go
make build         # ./impreza
./impreza --help
```

Run the unit tests:

```bash
make test
```

Lint (`gofmt` + `go vet`):

```bash
make lint
```

## Quickstart

```bash
# 1. Store credentials (same TOML shape as the Python CLI — config
#    files are interchangeable; both binaries read the same file).
impreza context create personal --key imp_... --secret ...

# 2. Confirm reachability (5-check health verification):
impreza doctor

# 3. Read commands across every resource:
impreza account info
impreza vps list
impreza domain check example.com mydomain.io
impreza catalog products --group "VPS"

# 4. Pipe into jq or yq for scripting (--output json | yaml):
impreza invoice list --output json | jq '[.[] | select(.status=="Unpaid")] | length'

# 5. Write verbs gate on confirm prompts; pass --yes / -y in scripts:
impreza vps reboot 17988
impreza vps proxmox snapshots create 17988 pre-update
impreza domain dns add example.com --type A --host www --value 1.2.3.4 --ttl 7200

# 6. Crypto top-up. --browser opens the BTCPay invoice URL;
#    --wait polls until the gateway confirms (default 2h timeout).
impreza account topup --amount 50 --method xmr --browser --wait
```

## Config

Reads + writes the same TOML config file as the Python CLI:

| OS      | Path |
|---------|------|
| Linux   | `$XDG_CONFIG_HOME/impreza/config.toml` (default `~/.config/impreza/config.toml`) |
| macOS   | `~/Library/Application Support/impreza/config.toml` |
| Windows | `%APPDATA%\impreza\config.toml` |

Override with `IMPREZA_CONFIG=/path/to/config.toml`. On POSIX, the
file is `chmod 0600` after every write.

This means you can install both CLIs side by side — they share state
and contexts. The binary higher in `$PATH` wins; pick whichever you
prefer per shell.

## Tab completion

Cobra ships completion scripts for bash, zsh, fish, and PowerShell:

```bash
# Bash
impreza completion bash > /etc/bash_completion.d/impreza
# zsh
impreza completion zsh > "${fpath[1]}/_impreza"
# fish
impreza completion fish > ~/.config/fish/completions/impreza.fish
# PowerShell
impreza completion powershell | Out-String | Invoke-Expression
```

Restart your shell, then `impreza <TAB>` suggests resource groups
and `impreza account <TAB>` suggests verbs.

## Stack

- Go 1.22+
- [`cobra`](https://cobra.dev/) — command framework
- [`BurntSushi/toml`](https://github.com/BurntSushi/toml) — config reader/writer
- [`fatih/color`](https://github.com/fatih/color) — palette helpers
- [`jedib0t/go-pretty`](https://github.com/jedib0t/go-pretty) — table rendering
- [`yaml.v3`](https://pkg.go.dev/gopkg.in/yaml.v3) — YAML output
- [`x/term`](https://pkg.go.dev/golang.org/x/term) — no-echo password prompts
- [`x/net/proxy`](https://pkg.go.dev/golang.org/x/net/proxy) — SOCKS5 (Tor)
- Hand-written HTTP client (oapi-codegen v2 doesn't yet handle
  OpenAPI 3.1's `type: [X, null]` form — see plan §7.2 sign-off
  for the codegen-pivot rationale).

## License

[MIT](../LICENSE) — © 2026 Impreza Host.
