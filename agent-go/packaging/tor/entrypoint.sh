#!/bin/sh
# impreza/tor entrypoint — self-patch from the Tor Project's apt repo on
# every (re)start, then exec the daemon.
#
# This is the roll path for upstream security releases: recreating or
# restarting this container pulls the latest tor build from
# deb.torproject.org. A failed update must never take hidden services down —
# the daemon starts with the baked-in version and the agent's heartbeat
# reports exactly which one it is.
set -u

# Ad-hoc commands (docker run ... tor --list-modules, sh, ...) skip the
# self-update and run directly.
if [ $# -gt 0 ]; then
  exec "$@"
fi

if ! timeout --signal=TERM --kill-after=5s 60s sh -c 'apt-get -o Acquire::Retries=1 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15 update -qq && DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=10 install -y -qq --only-upgrade tor'; then
  echo "impreza/tor: apt self-update skipped or failed; starting with the baked-in tor version" >&2
fi

# A tor package upgrade re-applies the package's owner (debian-tor, 2700) to
# /var/lib/tor, which is the agent's data mount. The daemon runs as root and
# refuses a data directory it does not own, so one upstream tor release would
# stop every start after it. Re-assert the owner on every start, after the
# upgrade and before the daemon reads the directory.
if ! chown root:root /var/lib/tor || ! chmod 0700 /var/lib/tor; then
  echo "impreza/tor: could not reset /var/lib/tor to root 0700; tor may refuse its data directory" >&2
fi

exec tor -f /etc/tor/torrc
