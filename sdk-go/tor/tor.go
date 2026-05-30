// Package tor provides a SOCKS5 dialer helper used by the Impreza SDK
// to route requests through Tor (or any SOCKS5 proxy).
//
// The package is intentionally tiny: one constant for the default Tor
// SOCKS port, and one constructor that turns a proxy URL into an
// http.RoundTripper. The rest of the SDK composes this with retry +
// auth transports.
package tor

import (
	"fmt"
	"net/http"
	"net/url"

	"golang.org/x/net/proxy"
)

// DefaultSOCKS is the address of the standard local Tor SOCKS5 port. Used
// when callers ask for Tor without specifying a proxy URL.
const DefaultSOCKS = "socks5://127.0.0.1:9050"

// Transport returns an http.RoundTripper that dials through the SOCKS5
// proxy at proxyURL. An empty proxyURL returns http.DefaultTransport
// unchanged — callers can therefore pass the result of a config lookup
// directly without an extra branch.
func Transport(proxyURL string) (http.RoundTripper, error) {
	if proxyURL == "" {
		return http.DefaultTransport, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL %q: %w", proxyURL, err)
	}

	// golang.org/x/net/proxy handles socks5:// transparently when the
	// URL scheme matches.
	dialer, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("build SOCKS5 dialer: %w", err)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	if d, ok := dialer.(proxy.ContextDialer); ok {
		tr.DialContext = d.DialContext
	} else {
		// Fallback for older proxy.Dialer implementations that don't
		// implement ContextDialer.
		tr.Dial = dialer.Dial //nolint:staticcheck // SA1019: graceful fallback
	}
	return tr, nil
}
