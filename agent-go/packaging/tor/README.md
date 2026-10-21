# impreza/tor image

The shared hidden-service daemon run by `impreza-agent` (`impreza_tor`,
one per host). Replaces the third-party `osminogin/tor-simple` with an
image whose Tor bits come only from the Tor Project.

- **Base**: official `debian:trixie-slim`, pinned by digest in the Dockerfile.
- **Tor**: installed from `deb.torproject.org`, signed with the official
  Tor Project apt key (`A3C4F0F9…886DDD89`). No vendored Tor, ever.
- **Fixed tor**: the image installs ONE exact tor package version,
  `TOR_VERSION` from `agent-go/packaging/tor/TOR_VERSION`, verified by apt's
  signed-repository check and an installed-version assertion at build. The
  version is recorded in `/usr/share/impreza-tor/package-version` and in the
  `org.opencontainers.image.version.tor-package` label. The tor apt source is
  removed from the image and the entrypoint never runs apt: the tor that starts
  is the one the agent's digest pin proves. If the file is missing or the
  version is no longer served by the repository, the build fails explicitly.
- **Updating tor**: bump `TOR_VERSION`, cut tag `tor-vX.Y.Z` (workflow below),
  verify the multi-arch manifest digest and pin it in the agent
  (`agent-go/internal/proxy/tor.go`, `TorImage`). Hosts get it on an explicit
  agent update; there is no in-place upgrade at start any more.
- **Data directory owner**: images before this change upgraded the tor package at
  start, which reset `/var/lib/tor` (the agent's data mount) to `debian-tor`.
  The daemon runs as root and refuses a directory it does not own, so the
  entrypoint still sets it back to `root` and `0700` on every start.
- **Proof**: `agent-go/packaging/tor/test-pinned.sh <TOR_VERSION>` (Docker):
  no-network start, version checks and the negative control against the
  previous entrypoint.

Built and published by `.github/workflows/tor-image.yml` (tag `tor-v<X>`,
multi-arch, GHCR). Release agents must reference the verified multiarchitecture
manifest digest. Version and major tags are discovery aliases, not immutable
identities. A new image digest takes effect on an explicit agent update;
`EnsureRunning` recreates the container while preserving identity bind mounts.
Pinning the image fixes its base, its entrypoint and its tor package.

Manual build (Linux amd64):

```sh
docker build --build-arg TOR_VERSION="$(cat agent-go/packaging/tor/TOR_VERSION)" -f agent-go/packaging/tor/Dockerfile -t ghcr.io/imprezahost/tor:1 .
```
