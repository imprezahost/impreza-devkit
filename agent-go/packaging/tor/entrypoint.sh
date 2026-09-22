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

exec tor -f /etc/tor/torrc
