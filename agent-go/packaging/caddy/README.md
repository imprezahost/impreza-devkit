# Caddy image (with Cloudflare DNS-01 module)

`Dockerfile` builds Caddy 2.x with `caddy-dns/cloudflare` compiled in.
Used by impreza-agent's reverse proxy (see `agent-go/internal/proxy/`)
so it can do ACME DNS-01 challenges against Cloudflare — necessary when
the Cloudflare proxy is on for the deployment's hostname (HTTP-01
breaks under proxy, DNS-01 doesn't).

## Build + push manually

```bash
# From repo root:
cd agent-go/packaging/caddy

# Local build for smoke (single-arch matching the host):
docker build -t ghcr.io/imprezahost/caddy:2-cf .

# Push (requires `docker login ghcr.io` with a PAT having package:write):
docker push ghcr.io/imprezahost/caddy:2-cf
```

## CI

`.github/workflows/caddy-image.yml` builds + pushes a multi-arch image
(linux/amd64, linux/arm64) on:

- **Tag push** matching `caddy-v<X.Y.Z>` — for cutting a new image
  release. Tag the upstream Caddy version (e.g. `caddy-v2.8.4`).
- **Manual dispatch** (`workflow_dispatch`) — for re-running an existing
  release if a build dependency changed.

Three GHCR tags are pushed each release:

| Tag | Stability | Use for |
|---|---|---|
| `ghcr.io/imprezahost/caddy:2.8.4-cf` | Immutable | Pin in production for reproducibility |
| `ghcr.io/imprezahost/caddy:2-cf` | Tracks latest 2.x-cf | What `agent-go/internal/proxy/caddy.go` uses |
| `ghcr.io/imprezahost/caddy:latest-cf` | Tracks newest cf-enabled | Local dev |

## Bumping the Caddy version

1. Edit `Dockerfile` → bump `ARG CADDY_VERSION=...` to the new upstream
   release.
2. Commit on a feature branch, push, open MR.
3. Once merged to `master`, push a tag `caddy-v<NEW_VERSION>` to fire
   the build pipeline.
4. Validate: `docker run --rm ghcr.io/imprezahost/caddy:<NEW_VERSION>-cf
   caddy version` should print the new version + `dns.providers.cloudflare`
   in `caddy list-modules`.

## Agent-side integration

`agent-go/internal/proxy/caddy.go` sets `Image = "ghcr.io/imprezahost/caddy:2-cf"`
and passes the deployment's CF API token via the `CADDY_CF_API_TOKEN`
env var on the Caddy container. The per-deployment Caddyfile fragment
references `{env.CADDY_CF_API_TOKEN}` only for the `tls { dns
cloudflare ... }` directive when the deploy targets an imprezaapps
subdomain. Apps hosted on customer-owned domains still use HTTP-01 by
default (no shared CF zone, no DNS-01 path).
