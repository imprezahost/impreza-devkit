# impreza/tor — the agent's shared hidden-service daemon.
#
# Trust model (revised C1, 2026-09-21): we never package Tor. The base is the
# official Debian image and tor comes from the Tor Project's own apt
# repository, verified with the official signing key — the same model the
# Tor Manager aaPanel plugin has run in production for years. Anyone can
# rebuild this Dockerfile and compare digests; the trust chain is Debian +
# Tor Project, not us.
FROM debian:trixie-slim@sha256:5bc3287b25407c965a30f38e32603dc253a3869e1b12a21ac09bfc27fd8b13ce

ARG TOR_REPO_KEY=A3C4F0F979CAA22CDBA8F512EE8CBC9E886DDD89

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && install -d /usr/share/keyrings \
 && curl --connect-timeout 15 --max-time 60 -fsSL "https://deb.torproject.org/torproject.org/${TOR_REPO_KEY}.asc" \
      | gpg --dearmor -o /usr/share/keyrings/tor-archive-keyring.gpg \
 && test "$(gpg --show-keys --with-colons /usr/share/keyrings/tor-archive-keyring.gpg | awk -F: '$1 == "fpr" {print $10; exit}')" = "$TOR_REPO_KEY" \
 && . /etc/os-release \
 && echo "deb [signed-by=/usr/share/keyrings/tor-archive-keyring.gpg] https://deb.torproject.org/torproject.org ${VERSION_CODENAME} main" \
      > /etc/apt/sources.list.d/tor.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends tor \
 && apt-get purge -y gnupg && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

COPY agent-go/packaging/tor/entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

# The daemon runs as root inside the container (the agent passes -u root) so
# Tor can chown the HiddenServiceDirs on the bind mounts; torrc mounts
# read-only at /etc/tor/torrc.
ENTRYPOINT ["/entrypoint.sh"]
