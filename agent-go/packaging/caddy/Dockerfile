# syntax=docker/dockerfile:1.6
#
# Custom Caddy build for impreza-agent (Phase 9.11d v2).
#
# Bundles:
#   - github.com/imprezahost/impreza-devkit/caddy-dns-impreza
#     A libdns provider that tunnels ACME DNS-01 challenges through the
#     operator's /v1/agent/dns-challenge/{present,cleanup} endpoints.
#     The provider uses the AGENT's own credentials (agent_id +
#     agent_secret) — NOT a Cloudflare token. The server validates
#     ownership + rate-limits per agent, then writes the TXT via its
#     own (IP-restricted) CF token. See caddy-dns-impreza/README.md
#     for the full threat-model rationale.
#
# Why not caddy-dns/cloudflare directly:
#   Shipping the operator's Cloudflare API token to every customer
#   agent makes it impossible to enforce IP restrictions and expands
#   the blast radius of a single VPS compromise to every subdomain on
#   the operator's zone. By proxying through our own API the CF token
#   never leaves the control plane.
#
# Build context layout assumed by COPY:
#   <build-ctx>/
#     ├── Dockerfile                  (this file)
#     ├── caddy-dns-impreza/          (the plugin source, monorepo subdir)
#     │   ├── go.mod
#     │   ├── go.sum
#     │   └── impreza.go
#     └── README.md
#
# Build locally for smoke (from impreza-devkit root):
#   docker build \
#     -t ghcr.io/imprezahost/caddy:2-cf \
#     -f agent-go/packaging/caddy/Dockerfile \
#     .
#
# Build via CI: tag `caddy-v<X.Y.Z>` triggers
# .github/workflows/caddy-image.yml, which pushes the multi-arch image
# to ghcr.io/imprezahost/caddy with three tags:
#   :<X.Y.Z>-cf   (immutable, exact version)
#   :2-cf         (major-version stream)
#   :latest-cf    (latest stable cf-enabled)
#
# Track upstream Caddy releases at https://github.com/caddyserver/caddy
# Bump CADDY_VERSION when there's a security release or a feature we
# need. The :2-cf stream tracks 2.x; 3.x would be a new image stream.

# Default tracks the latest 2.x stable. CI overrides this with the
# pinned X.Y.Z from the caddy-v<X.Y.Z> tag for reproducible releases.
# Local devs running `docker build .` without a build-arg get the
# rolling tip, which is fine for dev/smoke — production agents pull
# the pinned tag built by CI.
#
# History:
#   2.8.4 — broken: caddy/v2 imports zapslog.HandlerOptions which
#           depends on Go 1.21+ + a newer zap/exp combo that wasn't
#           present in this Caddy version. Build fails with
#           `undefined: zapslog.HandlerOptions`. Skip 2.8.x — use 2.9.x+.
ARG CADDY_VERSION=2

# ──────────────────────────── Builder ─────────────────────────────
#
# caddy:<ver>-builder-alpine ships Go + xcaddy. xcaddy compiles a
# fresh Caddy binary with the requested external modules baked in.
# We pin to alpine for size + reproducibility.
#
# The `--with <import-path>=<local-path>` form tells xcaddy to use a
# local source tree for the named module instead of resolving via
# `go get`. We copy our plugin into /build/caddy-dns-impreza first,
# then point xcaddy at it. This avoids needing the module's repo to
# be public (and avoids stale-cache issues during development).

FROM caddy:${CADDY_VERSION}-builder-alpine AS builder

# Plugin source — must match `module ...` declared in
# caddy-dns-impreza/go.mod (github.com/imprezahost/impreza-devkit/caddy-dns-impreza).
COPY caddy-dns-impreza /build/caddy-dns-impreza

RUN xcaddy build \
    --with github.com/imprezahost/impreza-devkit/caddy-dns-impreza=/build/caddy-dns-impreza

# ─────────────────────────── Final image ──────────────────────────
#
# Start from the matching caddy:<ver>-alpine base so all the rest of
# the image surface (entrypoint, default Caddyfile path, CMD, healthcheck
# semantics, exposed ports) matches what the agent already expects.
# Only the `caddy` binary itself is replaced.

FROM caddy:${CADDY_VERSION}-alpine

COPY --from=builder /usr/bin/caddy /usr/bin/caddy

# Smoke: `caddy list-modules` should include `dns.providers.impreza`.
# Catches a broken xcaddy build at image-build time instead of at first
# DNS-01 challenge attempt on a live deploy.
RUN caddy list-modules | grep -q '^dns\.providers\.impreza$' \
    || (echo 'FATAL: impreza DNS provider missing from xcaddy build' && exit 1)

LABEL org.opencontainers.image.source="https://github.com/imprezahost/impreza-devkit"
LABEL org.opencontainers.image.description="Caddy ${CADDY_VERSION} + caddy-dns-impreza, for impreza-agent ACME DNS-01 via proxied public API"
LABEL org.opencontainers.image.licenses="Apache-2.0"
