# impreza/tor — the agent's shared hidden-service daemon.
#
# Tor is installed from the Tor Project apt repository on an official Debian
# base, with the repository signing key fingerprint checked before installation.
# Runtime releases pin the verified multi-architecture image digest.
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
