# impreza/tor — the agent's shared hidden-service daemon.
#
# Tor is installed from the Tor Project apt repository on an official Debian
# base, with the repository signing key fingerprint checked before installation.
# Runtime releases pin the verified multi-architecture image digest.
#
# the image carries ONE exact tor package version, chosen at build time
# (TOR_VERSION, from agent-go/packaging/tor/TOR_VERSION) and verified by apt's
# signed-repository check plus an installed-version assertion. The container
# never runs apt at start: what the digest proves is what runs. A tor update is
# a new image (tag tor-vX.Y.Z) and a new digest pin in the agent.
FROM debian:trixie-slim@sha256:5bc3287b25407c965a30f38e32603dc253a3869e1b12a21ac09bfc27fd8b13ce

ARG TOR_REPO_KEY=A3C4F0F979CAA22CDBA8F512EE8CBC9E886DDD89
ARG TOR_VERSION

RUN case "${TOR_VERSION:-}" in ''|*[!A-Za-z0-9.+~:-]*) echo "FATAL: set TOR_VERSION to the exact Debian tor package version (agent-go/packaging/tor/TOR_VERSION)" >&2; exit 1;; esac \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && install -d /usr/share/keyrings \
 && curl --connect-timeout 15 --max-time 60 -fsSL "https://deb.torproject.org/torproject.org/${TOR_REPO_KEY}.asc" \
      | gpg --dearmor -o /usr/share/keyrings/tor-archive-keyring.gpg \
 && test "$(gpg --show-keys --with-colons /usr/share/keyrings/tor-archive-keyring.gpg | awk -F: '$1 == "fpr" {print $10; exit}')" = "$TOR_REPO_KEY" \
 && . /etc/os-release \
 && echo "deb [signed-by=/usr/share/keyrings/tor-archive-keyring.gpg] https://deb.torproject.org/torproject.org ${VERSION_CODENAME} main" \
      > /etc/apt/sources.list.d/tor.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends "tor=${TOR_VERSION}" \
 && test "$(dpkg-query -W -f='${Version}' tor)" = "$TOR_VERSION" \
 && apt-mark hold tor \
 && apt-get purge -y gnupg curl && apt-get autoremove -y \
 && rm -f /etc/apt/sources.list.d/tor.list \
 && rm -rf /var/lib/apt/lists/* \
 && install -d /usr/share/impreza-tor \
 && printf '%s\n' "$TOR_VERSION" > /usr/share/impreza-tor/package-version

COPY agent-go/packaging/tor/entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

LABEL org.opencontainers.image.version.tor-package="${TOR_VERSION}"

# The daemon runs as root inside the container (the agent passes -u root) so
# Tor can chown the HiddenServiceDirs on the bind mounts; torrc mounts
# read-only at /etc/tor/torrc.
ENTRYPOINT ["/entrypoint.sh"]
