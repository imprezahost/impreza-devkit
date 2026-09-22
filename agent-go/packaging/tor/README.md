# impreza/tor image

The shared hidden-service daemon run by `impreza-agent` (`impreza_tor`,
one per host). Replaces the third-party `osminogin/tor-simple` with an
image whose Tor bits come only from the Tor Project.

- **Base**: official `debian:trixie-slim`, pinned by digest in the Dockerfile.
- **Tor**: installed from `deb.torproject.org`, signed with the official
  Tor Project apt key (`A3C4F0F9…886DDD89`). No vendored Tor, ever.
- **Self-update**: the entrypoint runs `apt-get install --only-upgrade tor`
  at every container start, so an upstream security release reaches a host
  on any restart/recreation — no agent or image change required. Failure
  is bounded to 60 seconds (plus a 5-second termination grace period) and
  starts the baked version on failure; the agent heartbeat reports
  the running version either way.

Built and published by `.github/workflows/tor-image.yml` (tag `tor-v<X>`,
multi-arch, GHCR). Release agents must reference the verified multiarchitecture
manifest digest. Version and major tags are discovery aliases, not immutable
identities. A new image digest takes effect on an explicit agent update;
`EnsureRunning` recreates the container while preserving identity bind mounts.
Pinning the image fixes its base and entrypoint; signed Tor package updates at
startup remain subject to the update policy described above.

Manual build (Linux amd64):

```sh
docker build -f agent-go/packaging/tor/Dockerfile -t ghcr.io/imprezahost/tor:1 .
```
