# Build the complete Caddy binary from a reviewed module graph and checksums.
# This includes standard modules, the Impreza DNS provider and the Impreza
# Shield L7 package (Coraza WAF with the embedded OWASP CRS, proof-of-work
# gate, rate limiter). No Cloudflare credentials, no external rule downloads:
# the CRS rides pinned inside the Go module graph.
ARG CADDY_VERSION=2.11.4
FROM golang:1.26.6-alpine AS builder
ARG CADDY_VERSION
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOMAXPROCS=2
WORKDIR /src/agent-go/packaging/caddy/application
COPY caddy-dns-impreza /src/caddy-dns-impreza
COPY caddy-shield /src/caddy-shield
COPY agent-go/packaging/caddy/application ./
RUN go mod download \
    && test "$(go list -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)" = "v${CADDY_VERSION}" \
    && go build -mod=readonly -trimpath -ldflags="-s -w" -o /usr/bin/caddy .

FROM caddy:${CADDY_VERSION}-alpine
ARG CADDY_VERSION
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
RUN caddy list-modules | grep -q '^dns\.providers\.impreza$' \
    || (echo 'FATAL: Impreza DNS provider missing' && exit 1)
RUN caddy list-modules | grep -q '^http\.handlers\.waf$' \
    || (echo 'FATAL: Coraza WAF handler missing' && exit 1)
RUN caddy list-modules | grep -q '^http\.handlers\.shield_pow$' \
    || (echo 'FATAL: shield_pow handler missing' && exit 1)
RUN caddy list-modules | grep -q '^http\.handlers\.shield_rate_limit$' \
    || (echo 'FATAL: shield_rate_limit handler missing' && exit 1)
LABEL org.opencontainers.image.source="https://github.com/imprezahost/impreza-devkit"
LABEL org.opencontainers.image.description="Caddy ${CADDY_VERSION} with the Impreza DNS provider and Impreza Shield (Coraza WAF + PoW + rate limit)"
LABEL org.opencontainers.image.licenses="Apache-2.0"
