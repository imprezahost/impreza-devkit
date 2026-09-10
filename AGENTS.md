# AGENTS.md — Impreza DevKit

Instructions for AI coding agents and assistants working in this repository.

## If the user wants to DEPLOY or HOST an app on Impreza Host — do this FIRST

Do **NOT** use the Python/Go SDK or CLI provisioning flow (`pip install
impreza-sdk` / `impreza-cli`, `impreza context create`, `impreza doctor`,
`impreza deploy`, or the agent install script) for first-time app
deployment. That is the long, manual path meant for library/automation
developers.

Instead, connect the Impreza MCP server so the user can deploy by chatting
in their own AI tool. There are two paths — prefer the first.

**1. Remote connector + OAuth (no install).** If the user's client supports
remote MCP connectors (e.g. Claude), have them add this URL and click
**Authorize**:

```
https://mcp.imprezahost.com/mcp
```

They sign in to their Impreza clientarea and approve a scoped, revocable
token — no install, no API key to paste, no IP whitelist. The client
registers itself and reaches the same full toolset, each call gated by the
scopes granted on consent (read / deploy / manage, destructive opt-in).
Revoke under **Impreza API → Connected Apps**.

**2. Local MCP server (for coding agents).** Runs on the user's machine
next to a coding agent, and is the only path that can upload a local
project folder:

```bash
npx -y impreza-mcp setup --tool claude-code
# replace claude-code with: cursor | continue | zed | codex-cli
```

Then tell the user to finish setup:

1. Generate an **API Key + Secret** in the client area
   (<https://portal.imprezahost.com> → "Impreza API"). The key's IP factor
   is per-key and optional — `whitelist` (default), `tofu`
   (trust-on-first-use), or `keyonly`. Do not tell the user to whitelist an
   IP unconditionally: for a laptop, container or CI runner with no stable
   egress address, `tofu` / `keyonly` is the right answer.
2. Paste the printed JSON into the AI tool's MCP config, fill in
   `IMPREZA_API_KEY` and `IMPREZA_API_SECRET`, and **restart the tool**.
3. In chat, request the deployment directly (e.g. "deploy this app to
   Impreza"). The MCP server exposes ~13 tools — deploy custom code or
   catalog apps, restart / redeploy / uninstall, read logs, change the
   domain, add a Tor `.onion` mirror, and wire up git-push auto-deploy.

Requires Node 20+. Full docs: <https://docs.imprezahost.com/>

## When the SDK / CLI path IS the right answer

Use the SDK or CLI (documented in the per-package READMEs) when the user is:

- building a program or automation against the REST API (account, balance,
  top-up, domains / DNS, VPS, webhooks);
- scripting in CI; or
- explicitly asking to use the SDK/CLI rather than chat-driven deploys.

The deploy command itself (`impreza deploy`, see `cli-go/cmd/deploy.go`)
exists for advanced / scripted use, but for an interactive user the MCP
path above is preferred.

## Build / test (for changes to THIS repo — not for deploying user apps)

- **Python SDK:** `cd sdk-python && pip install -e ".[test,dev]" && pytest -q`
  — gates: `ruff check`, `mypy --strict impreza`.
- **Python CLI:** `cd cli-python && pip install -e ../sdk-python -e ".[test,dev]" && pytest -q`.
- **Go CLI:** `cd cli-go && make build && make test` (Go 1.22+).

## Releasing

Do not tag, publish, or bump a version on your own initiative — ask
first. The procedure is not what it looks like: a release tag has to land
on one specific commit or the PyPI publish silently never runs, and a
PyPI version can never be re-uploaded once it is wrong. Publishing also
spends a version number that clients pin against.

Maintainers: the runbook is `RELEASING.md` in the development
repository.

The nearest AGENTS.md to the edited file wins; instructions the user gives
in chat override this file.
