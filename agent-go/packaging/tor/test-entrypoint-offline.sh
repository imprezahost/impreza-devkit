#!/bin/sh
# the entrypoint's start path, run for real WITHOUT Docker or network.
#   agent-go/packaging/tor/test-entrypoint-offline.sh [base-commit]
# The actual entrypoint.sh runs under sh with stub commands first on PATH: apt-get/apt/dpkg are tripwires that
# leave a marker, tor records how it was exec'd, chown/chmod only record (nothing on this machine is touched).
# 1. the candidate entrypoint never calls the package manager and execs tor with the mounted torrc;
# 2. negative control: the base entrypoint calls apt-get before tor;
# 3. the Dockerfile's TOR_VERSION guard refuses an empty or malformed value and accepts a Debian version.
set -eu
BASE=${1:-48c2a8f0a64b0844799685052a8109a896e3a245}
ROOT=$(git rev-parse --show-toplevel)
FAILED=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

stubs="$work/bin"
mkdir -p "$stubs"
for cmd in apt-get apt dpkg; do
  printf '#!/bin/sh\necho "%s $*" >> "%s/package-manager-called"\nexit 1\n' "$cmd" "$work" > "$stubs/$cmd"
done
printf '#!/bin/sh\necho "tor $*" > "%s/tor-exec"\nexit 0\n' "$work" > "$stubs/tor"
for cmd in chown chmod; do
  printf '#!/bin/sh\necho "%s $*" >> "%s/ownership"\nexit 0\n' "$cmd" "$work" > "$stubs/$cmd"
done
chmod 0755 "$stubs"/*

run_entrypoint() { # $1 = entrypoint file
  rm -f "$work/package-manager-called" "$work/tor-exec" "$work/ownership"
  PATH="$stubs:$PATH" sh "$1" > "$work/out" 2>&1 || true
}

run_entrypoint "$ROOT/agent-go/packaging/tor/entrypoint.sh"
[ ! -e "$work/package-manager-called" ] && pass 'candidate entrypoint never calls the package manager' \
  || fail "candidate entrypoint called: $(cat "$work/package-manager-called")"
[ "$(cat "$work/tor-exec" 2>/dev/null)" = 'tor -f /etc/tor/torrc' ] && pass 'candidate entrypoint execs tor with the mounted torrc' \
  || fail 'candidate entrypoint did not exec tor -f /etc/tor/torrc'
grep -q 'chown root:root /var/lib/tor' "$work/ownership" 2>/dev/null && grep -q 'chmod 0700 /var/lib/tor' "$work/ownership" \
  && pass 'candidate entrypoint still re-asserts root 0700 on /var/lib/tor' || fail 'candidate entrypoint lost the data-dir ownership reset'

git -C "$ROOT" show "$BASE:agent-go/packaging/tor/entrypoint.sh" > "$work/old-entrypoint.sh"
run_entrypoint "$work/old-entrypoint.sh"
grep -q '^apt-get .*update' "$work/package-manager-called" 2>/dev/null \
  && pass 'negative control: the base entrypoint calls apt-get before tor' || fail 'negative control: the base entrypoint did not call apt-get'

guard=$(sed -n 's/^RUN \(case "\${TOR_VERSION:-}" in .*esac\) \\$/\1/p' "$ROOT/agent-go/packaging/tor/Dockerfile")
[ -n "$guard" ] || { fail 'Dockerfile TOR_VERSION guard not found'; exit 1; }
PIN=$(tr -d '[:space:]' < "$ROOT/agent-go/packaging/tor/TOR_VERSION")
for bad in '' "$PIN;rm" "$PIN 1" '../x'; do
  if TOR_VERSION="$bad" sh -c "$guard" >/dev/null 2>&1; then fail "TOR_VERSION guard accepted: '$bad'"; else pass "TOR_VERSION guard refuses: '$bad'"; fi
done
TOR_VERSION="$PIN" sh -c "$guard" >/dev/null 2>&1 && pass "TOR_VERSION guard accepts the pinned version ($PIN)" \
  || fail 'TOR_VERSION guard refused the pinned version'

exit $FAILED
