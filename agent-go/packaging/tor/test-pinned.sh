#!/bin/sh
# Pinned-version proof (needs Docker in a build environment; never run on a customer host).
#   agent-go/packaging/tor/test-pinned.sh <TOR_VERSION> [base-commit]
# 1. the pinned image keeps tor running with NO network at all (no mirror can be reached);
# 2. the version that runs is the pinned one (dpkg, the baked record and `tor --version`);
# 3. no apt source for tor is left in the image;
# 4. negative control: the base entrypoint, in the same image and the same no-network box, tries apt first.
# Uses DisableNetwork in a throwaway torrc: it proves the start path, not Tor circuits.
set -eu
V=${1:?usage: test-pinned.sh <TOR_VERSION> [base-commit]}
BASE=${2:-48c2a8f0a64b0844799685052a8109a896e3a245}
ROOT=$(git rev-parse --show-toplevel)
IMG=impreza-tor-x720:test
FAILED=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
work=$(mktemp -d)
cleanup() { docker rm -f x720-new x720-old >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT INT TERM

docker build -q --build-arg TOR_VERSION="$V" -f "$ROOT/agent-go/packaging/tor/Dockerfile" -t "$IMG" "$ROOT" >/dev/null
printf 'DataDirectory /var/lib/tor\nDisableNetwork 1\nSocksPort 0\nLog notice stdout\n' > "$work/torrc"
chmod 0644 "$work/torrc"
# a tripwire over apt-get in the container. A call leaves a marker, so "no apt at start" is observed, not grepped.
printf '#!/bin/sh\ntouch /tmp/apt-get-called\nexit 1\n' > "$work/apt-get"
chmod 0755 "$work/apt-get"
TRIP="-v $work/apt-get:/usr/bin/apt-get:ro"

docker run -d --name x720-new --network none -u root $TRIP -v "$work/torrc:/etc/tor/torrc:ro" "$IMG" >/dev/null
sleep 5
[ "$(docker inspect -f '{{.State.Running}}' x720-new)" = true ] && pass 'pinned image keeps tor running without any network' || fail 'pinned image did not keep tor running without network'
# docker cp also reads a stopped container, so a tor that exited cannot turn the tripwire into a false pass.
docker inspect x720-new >/dev/null || fail 'pinned container missing'
if docker cp x720-new:/tmp/apt-get-called "$work/new-marker" 2>/dev/null; then fail 'pinned entrypoint called apt-get'; else pass 'pinned entrypoint never calls apt-get (tripwire)'; fi

[ "$(docker exec x720-new dpkg-query -W -f='${Version}' tor)" = "$V" ] && pass 'installed tor package is the pinned version' || fail 'installed tor package differs from TOR_VERSION'
[ "$(docker exec x720-new cat /usr/share/impreza-tor/package-version)" = "$V" ] && pass 'baked version record matches' || fail 'baked version record differs'
UPSTREAM=${V%%-*}
docker exec x720-new tor --version | head -n 1 | grep -qF "Tor version $UPSTREAM" && pass 'tor --version reports the pinned upstream version' || fail 'tor --version does not report the pinned version'
if docker exec x720-new sh -c 'test -e /etc/apt/sources.list.d/tor.list'; then fail 'tor apt source left in the image'; else pass 'no tor apt source left'; fi

git -C "$ROOT" show "$BASE:agent-go/packaging/tor/entrypoint.sh" > "$work/old-entrypoint.sh"
chmod 0755 "$work/old-entrypoint.sh"
docker run -d --name x720-old --network none -u root $TRIP -v "$work/torrc:/etc/tor/torrc:ro" -v "$work/old-entrypoint.sh:/entrypoint.sh:ro" "$IMG" >/dev/null
tried=0
i=0
while [ $i -lt 90 ]; do
  if docker cp x720-old:/tmp/apt-get-called "$work/old-marker" 2>/dev/null; then tried=1; break; fi
  sleep 1; i=$((i + 1))
done
[ $tried -eq 1 ] && pass 'negative control: the base entrypoint tries the network (apt) before tor' || fail 'negative control did not show the base entrypoint trying apt'

exit $FAILED
