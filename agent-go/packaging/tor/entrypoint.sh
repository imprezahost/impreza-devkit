#!/bin/sh
# impreza/tor entrypoint — exec the daemon baked into this image.
#
# no package manager runs here. The tor that starts is the exact package
# version fixed at image build (/usr/share/impreza-tor/package-version), the
# one the agent's digest pin proves; an upstream tor release reaches hosts as a
# new image (tag tor-vX.Y.Z) and a new digest in the agent, never as an
# in-place upgrade from whichever mirror answers at start.
set -u

# Ad-hoc commands (docker run ... tor --list-modules, sh, ...) run directly.
if [ $# -gt 0 ]; then
  exec "$@"
fi

# /var/lib/tor is the agent's data mount. Images before this change upgraded the tor
# package at start, which re-applied the package owner (debian-tor, 2700) to it;
# the daemon runs as root and refuses a data directory it does not own. Keep
# re-asserting root 0700 so a host coming from such an image keeps starting.
if ! chown root:root /var/lib/tor || ! chmod 0700 /var/lib/tor; then
  echo "impreza/tor: could not reset /var/lib/tor to root 0700; tor may refuse its data directory" >&2
fi

exec tor -f /etc/tor/torrc
