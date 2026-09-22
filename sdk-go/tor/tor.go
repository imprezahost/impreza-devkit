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
	"net"
	"net/http"
	"net/url"
	"time"

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
		return nil, fmt.Errorf("invalid SOCKS proxy URL")
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, fmt.Errorf("proxy must use SOCKS5")
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid SOCKS proxy URL")
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return nil, fmt.Errorf("SOCKS proxy requires host and port")
	}
	u.Scheme = "socks5"

	// golang.org/x/net/proxy handles socks5:// transparently when the
	// URL scheme matches.
	dialer, err := proxy.FromURL(u, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("build SOCKS5 dialer: %w", err)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // Explicit SOCKS must not inherit HTTP(S)_PROXY from the environment.
	if d, ok := dialer.(proxy.ContextDialer); ok {
		tr.DialContext = d.DialContext
	} else {
		return nil, fmt.Errorf("SOCKS transport requires context cancellation")
	}
	return tr, nil
}
