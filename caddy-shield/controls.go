package caddyshield

import (
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Consult the actual transport peer only. Forwarded headers are untrusted.
// Tor's private socket never has trusted sources in the agent's onion render.
func trustedPeer(r *http.Request, sources []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	peer = peer.Unmap()
	for _, source := range sources {
		if source.Contains(peer) {
			return true
		}
	}
	return false
}

func validateControlFields(paths, sources []string, deadline int64) ([]netip.Prefix, error) {
	if err := sdkclient.ValidateShieldControls(&sdkclient.ShieldControls{PowPaths: paths, TrustedSources: sources, AttackExpiresAt: deadline}, time.Now()); err != nil {
		return nil, err
	}
	return sdkclient.ParseShieldTrustedSources(sources)
}

func (m *PowMiddleware) attackActive() bool {
	return m.AttackUntil > time.Now().Unix()
}

// Bind cookies to this activation. A previously solved weaker challenge
// cannot be replayed to bypass attack mode. No visitor-derived state is added.
func (m *PowMiddleware) cookieScope() string {
	if m.attackActive() {
		return m.Deployment + "|attack:" + strconv.FormatInt(m.AttackUntil, 10)
	}
	return m.Deployment
}

func powPathMatches(raw string, paths []string) bool {
	if len(paths) == 0 {
		return true
	}
	for _, prefix := range paths {
		if strings.HasPrefix(raw, prefix) {
			return true
		}
	}
	return false
}
