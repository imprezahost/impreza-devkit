# `impreza-cli` — Official CLI for Impreza Host

Command-line interface for the Impreza Host public REST API.
Built on top of [`impreza-sdk`](../sdk-python/README.md) — same
auth model, same Tor support, same retry behaviour, plus
multi-context configuration and Rich-rendered tables for human-
friendly output.

```bash
pip install impreza-cli
```

Requires Python 3.10+. See [`../CHANGELOG.md`](../CHANGELOG.md) for
release history.

## Quickstart

```bash
# 1. Add a context with your API credentials. Generate keys in
#    Impreza Account → API Keys; whitelist the calling
#    machine's IP at the same screen.
$ impreza context create personal --key imp_... --secret ...
Context 'personal' created and set as default.

# 2. Confirm everything works. impreza doctor runs five sequenced
#    health checks (config, API reachable, key status, IP
#    whitelist, account profile) and exits 0 only if all pass.
$ impreza doctor

impreza doctor
----------------------------------------
[OK]    active-context: Default context
[OK]    api-reachable: GET /account/api-keys/self OK (142ms)
        key prefix='imp_a1b2c3d4', label='devkit'
[OK]    key-status: status='active'
[OK]    ip-whitelist: request_ip 200.1.2.3 matches entry ('home')
[OK]    account-profile: Jane Doe <jane@example.com>, balance 5.00 USD
                         registered 2024-01-15
----------------------------------------
All checks passed. 5/5.

# 3. Read commands span every resource group:
$ impreza account info             # profile + balance
$ impreza vps list                 # across both backends
$ impreza domain check example.com mydomain.io
$ impreza catalog products --group "VPS"

# 4. Pipe into jq for scripting (every read verb supports --output
#    json | yaml):
$ impreza invoice list --output json \
    | jq '[.[] | select(.status == "Unpaid")] | length'

# 5. Write verbs are gated by confirm_or_exit so you don't lose
#    data accidentally; pass --yes / -y to skip prompts in scripts:
$ impreza vps reboot 17988
$ impreza vps proxmox snapshots create 17988 pre-update
$ impreza domain dns add example.com --type A --name www --value 1.2.3.4

# 6. Crypto top-up. --browser opens the BTCPay invoice URL
#    automatically; --wait polls until the gateway confirms
#    (default 2h timeout matches server-side invoice expiry).
$ impreza account topup --amount 50 --method xmr --browser --wait
```

## Authentication

Two ways to authenticate. The CLI tries them in order:

1. **Context** (recommended) — `impreza context create <name>` stores
   credentials in a config file; commands read them automatically.
   Per-invocation override via `impreza --context other <command>`.

2. **Environment variables** — `IMPREZA_API_KEY` + `IMPREZA_API_SECRET`.
   Useful in CI, but contexts are preferred for local work.

The config file lives at:

| OS | Path |
|---|---|
| Linux | `$XDG_CONFIG_HOME/impreza/config.toml` (default `~/.config/impreza/config.toml`) |
| macOS | `~/Library/Application Support/impreza/config.toml` |
| Windows | `%APPDATA%\impreza\config.toml` |

Override with `IMPREZA_CONFIG=/path/to/config.toml` for testing or
non-standard layouts.

On POSIX, the config file is `chmod 0o600` after every write so only
the owner can read the credentials. Windows ACLs are left to the OS
default.

## Commands

The CLI groups commands by resource. Run `impreza <group> --help`
to see the full subcommand list, or `impreza <group> <command>
--help` for option-level detail.

| Group | Verbs | Notes |
|---|---|---|
| `context` | `create / use / list / current / delete` | Local credential management — never hits the network |
| `doctor` | (single command) | Health check — config + API reachable + key status + IP whitelist + account profile |
| `account` | `info / balance / services / topup / topup-status` | Profile + balance + services + crypto top-up |
| `catalog` | `products / product / product-groups / tlds` | Pre-purchase discovery |
| `domain` | `show / check / pricing / register / transfer / set-nameservers / lock / unlock / id-protection / raa-verify / gdpr-auth / transfer-approval` + `domain dns list / add / update / delete / activate` | Domain registrations + full DNS CRUD |
| `vps` | `list / show / status / start / stop / reboot / shutdown / set-hostname / set-password / reinstall / migrate / cancel` + `vps proxmox snapshots / backups / backup-schedules / network` + `vps cloud images / rescue / iso / ssh-keys / vnc / vnc-password / resize / boot-order / ipv6` | Cross-backend (Proxmox + Cloud) VPS with smart dispatch |
| `order` | `list / show / create / upgrade` | Submit / browse product orders |
| `service` | `cancel` | Submit cancellation request (any service) |
| `webhook` | `list / show / create / update / delete / rotate-secret / deliveries / event-types` | Webhook subscription management + delivery history. Event-payload contract documented in [`../openapi/asyncapi.yaml`](../openapi/asyncapi.yaml). |
| `invoice` | `list / show` | Invoices with line items + transactions |
| `key` | `whoami` | Active API key identity + IP whitelist |

**Conventions:**

- Destructive verbs prompt for confirmation; pass `--yes` / `-y`
  to skip the prompt in scripts.
- Operation-returning verbs (`vps reinstall`, `vps migrate`,
  `vps proxmox snapshots rollback`, `vps proxmox backups
  create/restore`) accept `--wait` to block on the Proxmox queue,
  with `--timeout` (default 600 s for fast ops, 1800 s for the
  slower restores).
- Cost-incurring verbs (`domain register/transfer/id-protection`,
  `order create/upgrade`, `account topup`) call out the
  balance impact in the confirmation prompt; an
  `InsufficientCredit` 402 surfaces with a hint pointing at
  `impreza account topup`.
- Verbs that mutate a resource emit a green success line on
  stdout; queued / reboot-required state changes emit a cyan
  info line. Errors are red on stderr.

**Service termination policy:** `service cancel` / `vps cancel`
submit an `AddCancelRequest` — staff approves the actual
termination. There is no direct customer path to terminate a
service or remove a service suspension (suspension is
billing-state and is removed automatically when the overdue
invoice is paid, or manually by staff after an abuse hold is
resolved).

## Output formats

Every command supports `--output table|json|yaml` (short form `-o`).

| Format | Default | Best for |
|---|---|---|
| `table` | yes | human reading at the terminal |
| `json` | | piping into `jq`, automation, scripting |
| `yaml` | | human-editable config snapshots, CI/CD pipelines |

YAML output requires the optional `pyyaml` dependency:

```bash
pip install impreza-cli[yaml]
```

The CLI raises a clear `RuntimeError` pointing at the install hint
if you select `--output yaml` without it.

The flag works at both the global level and per-command:

```bash
# Global default for the invocation
impreza --output json account info

# Per-command override (wins over global)
impreza --output yaml account info --output table
```

## Tab completion

Typer ships completion for `bash`, `zsh`, `fish`, and PowerShell
out of the box:

```bash
# Install for the current shell (auto-detected)
impreza --install-completion

# Or explicitly
impreza --install-completion bash    # / zsh / fish / powershell

# Inspect the script before installing
impreza --show-completion bash
```

After installing, restart the shell (or `source ~/.bashrc` /
equivalent) and `impreza <TAB>` should suggest resource groups,
`impreza account <TAB>` should suggest verbs, and so on.

## Tor

Inherited from the SDK. Three knobs:

```bash
# Per-context override at create time
impreza context create offshore \
  --key imp_... --secret ... \
  # No --proxy flag yet; for now, set IMPREZA_USE_TOR before invoking

# Env var, picked up by the SDK transparently
IMPREZA_USE_TOR=1 impreza account info

# Programmatic via the SDK (Python users skip the CLI for this)
```

The SDK's `auto_tor=True` path (probe Tor, fall back to clearnet)
isn't surfaced through the CLI yet — coming in a future release
alongside the `--via-tor` shortcut.

## Error handling

The CLI maps SDK exceptions to friendly stderr messages and a
non-zero exit code, matching the format `ImprezaError.__str__`
produces:

```
Error: Invalid API credentials. (code=UNAUTHORIZED) [request_id=req_abc]
```

Tracebacks never leak from expected failures (auth errors, missing
contexts, 404s, 429s, etc.). Bugs in the CLI itself still raise so
the traceback isn't swallowed — that's intentional.

## Development

```bash
git clone https://github.com/imprezahost/impreza-devkit.git
cd impreza-devkit/cli-python

python -m venv .venv
# Linux/macOS:        source .venv/bin/activate
# Windows PowerShell: .venv\Scripts\Activate.ps1

# Install editable + test/dev/yaml extras + the SDK as a path dep
pip install -e ../sdk-python -e ".[test,dev,yaml]"

pytest                              # unit + Typer-runner E2E
ruff check
mypy --strict impreza_cli
```

To run the live integration smokes (skipped silently without creds):

```bash
export IMPREZA_API_KEY="imp_..."
export IMPREZA_API_SECRET="..."
# Optional, for `impreza domain show / dns list`:
export IMPREZA_TEST_DOMAIN="<a domain on your account>"

pytest -v -s tests/
```

The smokes exercise the same surface as the unit tests against the
real API, so they catch contract drift between the CLI and the
server.

## License

MIT. See [`../LICENSE`](../LICENSE) at the repository root.
