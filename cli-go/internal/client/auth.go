// Package client builds the HTTP client + auth headers used by every
// resource command.
//
// Phase 7.2 layers `oapi-codegen`-free hand-written resource clients
// on top of this. The transport chain is:
//
//	retryTransport → authTransport → (SOCKS5 proxy if Tor) → net/http
package client

import (
	"fmt"
	"net/http"
	"net/url"

	"golang.org/x/net/proxy"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/config"
)

// DefaultBaseURL is the live API endpoint. Overridable per-context via
// the context's URL field (set with `--url` on `context create`).
const DefaultBaseURL = "https://api.imprezahost.com"

// authTransport wraps an http.RoundTripper to inject the two required
// authentication headers on every outbound request. Mirrors the
// Python SDK's HMAC-free header-auth scheme exactly (key + secret in
// separate headers; the server keys IP whitelist on the secret hash).
type authTransport struct {
	base   http.RoundTripper
	key    string
	secret string
}

// RoundTrip implements http.RoundTripper. Headers are set on a clone
// of the request so we don't mutate a caller-owned `*http.Request`.
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("X-API-Key", t.key)
	clone.Header.Set("X-API-Secret", t.secret)
	clone.Header.Set("User-Agent", userAgent())
	return t.base.RoundTrip(clone)
}

// userAgent returns the User-Agent string sent on every request. Format
// matches what the Python SDK sends (`impreza-cli/<version>`), so
// server-side request logs can disambiguate Python vs Go callers.
func userAgent() string {
	return "impreza-cli-go/" + version
}

// version is set by the cmd package at startup (via cmd.SetVersion in
// main). Default keeps the User-Agent useful for dev builds even if
// nothing wires it.
var version = "dev"

// SetVersion is called from cmd.SetVersion so the client embeds the
// same version string the user sees from `impreza --version`.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}

// buildTransport returns the base RoundTripper for a context. Honours
// the context's Tor / Proxy settings:
//
//   - If ctx.Proxy is set, dial through that SOCKS5 URL (any host:port).
//   - Else if ctx.UseTor is true, dial through socks5://127.0.0.1:9050
//     (the standard Tor SOCKS port).
//   - Else use http.DefaultTransport.
func buildTransport(ctx config.Context) (http.RoundTripper, error) {
	proxyURL := ctx.Proxy
	if proxyURL == "" && ctx.UseTor {
		proxyURL = "socks5://127.0.0.1:9050"
	}
	if proxyURL == "" {
		return http.DefaultTransport, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL %q: %w", proxyURL, err)
	}

	// `golang.org/x/net/proxy` handles socks5:// transparently when the
	// URL scheme matches.
	dialer, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("build SOCKS5 dialer: %w", err)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	if d, ok := dialer.(proxy.ContextDialer); ok {
		tr.DialContext = d.DialContext
	} else {
		// Fallback for older proxy.Dialer implementations.
		tr.Dial = dialer.Dial //nolint:staticcheck // SA1019: graceful fallback
	}
	return tr, nil
}

// BaseURL returns the API base URL for a context (its override, or the
// shipped default if unset).
func BaseURL(ctx config.Context) string {
	if ctx.URL != "" {
		return ctx.URL
	}
	return DefaultBaseURL
}
