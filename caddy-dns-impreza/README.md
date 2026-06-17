# caddy-dns-impreza

Caddy DNS provider plugin for the Impreza Platform's ACME DNS-01 challenge proxy.

## Why not just use `caddy-dns/cloudflare`?

The Impreza Platform's auto-subdomain feature issues certs for the operator-managed `imprezaapps.com` zone (and friends). Shipping the Cloudflare API token to every customer agent so it can solve DNS-01 directly is a non-starter:

- The token's blast radius is "every subdomain on imprezaapps.com" — exposure on **one** customer's VPS would expose **every other customer**.
- Cloudflare tokens can be IP-restricted, but agents run on customer-controlled VPSes with IPs unknown ahead of time.
- The operator already invests in a public API surface (`/v1/agent/*`) with per-agent credentials + rate-limiting + observability. Tunneling DNS challenges through that surface is the same trust model already in place.

This plugin sits between Caddy's ACME client and the Impreza public API. Caddy still does DNS-01 — but instead of talking to Cloudflare directly, it asks the operator's API server to perform the TXT record dance on its behalf. The CF token never leaves the operator's infrastructure.

```
Caddy ─┐
       │  /v1/agent/dns-challenge/present
       │  /v1/agent/dns-challenge/cleanup
       │  X-Agent-Id + X-Agent-Secret
       ▼
Impreza API ─┐
             │  validates: agent owns the deployment + domain
             │  rate-limits per agent_id
             │  audits every present/cleanup
             ▼
Cloudflare API (token stays here, IP-restricted to portal)
```

The agent's `agent_id` + `agent_secret` are the **same credentials** it already uses for `/v1/agent/poll`, `/report`, `/deploy-result`. No new auth surface; no new secret to rotate.

## Caddyfile syntax

Minimal:

```caddy
example.com {
    tls {
        dns impreza {env.IMPREZA_AGENT_ID} {env.IMPREZA_AGENT_SECRET}
    }
    reverse_proxy backend:8080
}
```

With a non-default base URL (staging / self-hosted):

```caddy
example.com {
    tls {
        dns impreza {
            agent_id {env.IMPREZA_AGENT_ID}
            agent_secret {env.IMPREZA_AGENT_SECRET}
            base_url https://your-control-plane.example.com
        }
    }
    reverse_proxy backend:8080
}
```

The `impreza-agent` daemon writes `IMPREZA_AGENT_ID`, `IMPREZA_AGENT_SECRET`, and `IMPREZA_API_URL` into the Caddy container's env-file at proxy startup time, so the values above are resolved from there.

## Building

Bundled into the impreza-agent Caddy image via `xcaddy build`:

```
xcaddy build --with github.com/imprezahost/impreza-devkit/caddy-dns-impreza=./caddy-dns-impreza
```

See `agent-go/packaging/caddy/Dockerfile` for the canonical build.

## Server-side contract

The two endpoints this plugin calls live in `imprezaapi`. Wire spec:

```
POST /v1/agent/dns-challenge/present
Headers:  X-Agent-Id, X-Agent-Secret, Content-Type: application/json
Body:     { "fqdn": "vault-abc.imprezaapps.com", "value": "<acme-challenge-token>" }
Success:  200 { "fqdn": "...", "record_name": "_acme-challenge.<fqdn>" }
Errors:   400 INVALID_REQUEST   — missing/empty fqdn or value
          401 UNAUTHORIZED      — credentials missing/invalid
          403 FORBIDDEN         — agent doesn't own a deployment with that fqdn,
                                   OR fqdn isn't in an operator-managed zone
          429 RATE_LIMITED      — > N challenges per hour for this agent
          502 UPSTREAM_ERROR    — Cloudflare API rejected the request

POST /v1/agent/dns-challenge/cleanup
Headers:  X-Agent-Id, X-Agent-Secret, Content-Type: application/json
Body:     { "fqdn": "vault-abc.imprezaapps.com" }
Success:  204 (no body) — record deleted OR did not exist (idempotent)
Errors:   400 / 401 / 403 same as present, looser ownership check on cleanup
                                   (we want stale records gone even if the
                                   deployment row has already been deleted).
```

## Limitations

- TXT records only — Caddy only emits TXT for ACME DNS-01. Other libdns operations are no-ops or return empty.
- No `GetRecords` support — server doesn't expose record listing (intentional: minimum surface).
- TTL is server-decided (120s on the Cloudflare side) — clients can't customize.
- Cleanup is best-effort. The plugin won't fail a renewal because cleanup of a prior challenge record failed.
