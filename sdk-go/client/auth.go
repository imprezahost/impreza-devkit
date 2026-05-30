// Package client builds the HTTP client + auth headers used by every
// resource method.
//
// The transport chain is:
//
//	retryTransport → authTransport → (SOCKS5 proxy if Tor/Proxy set) → net/http
package client

import (
	"net/http"

	"github.com/imprezahost/impreza-devkit/sdk-go/config"
	"github.com/imprezahost/impreza-devkit/sdk-go/tor"
)

// DefaultBaseURL is the live API endpoint. Overridable per-context via
// the context's URL field.
const DefaultBaseURL = "https://api.imprezahost.com"

// Standard header names for the two auth realms the SDK speaks.
//
//   - apiKey realm:  X-API-Key + X-API-Secret  (used by /v1/* CLI/clients)
//   - agent realm:   X-Agent-Id + X-Agent-Secret  (used by /v1/agent/*)
const (
	headerAPIKey      = "X-API-Key"
	headerAPISecret   = "X-API-Secret"
	headerAgentID     = "X-Agent-Id"
	headerAgentSecret = "X-Agent-Secret"
)

// authTransport wraps an http.RoundTripper to inject the two required
// authentication headers on every outbound request. The header *names*
// are configurable so the same transport carries both the API-key
// realm (CLI) and the agent realm (impreza-agent) without branching.
type authTransport struct {
	base      http.RoundTripper
	keyHeader string
	secHeader string
	keyValue  string
	secValue  string
}

// RoundTrip implements http.RoundTripper. Headers are set on a clone
// of the request so we don't mutate a caller-owned `*http.Request`.
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set(t.keyHeader, t.keyValue)
	clone.Header.Set(t.secHeader, t.secValue)
	clone.Header.Set("User-Agent", userAgent())
	return t.base.RoundTrip(clone)
}

// userAgent returns the User-Agent string sent on every request. The
// prefix identifies which binary is calling (CLI, agent, or unset SDK
// consumer); the version is the build semver. Server-side request logs
// can disambiguate callers without parsing payload shape.
func userAgent() string {
	return uaPrefix + "/" + version
}

// uaPrefix identifies the calling binary. Defaults to "impreza-sdk-go"
// so external consumers of the SDK get a useful UA without any setup;
// the CLI and agent override this via SetUserAgent at startup.
var uaPrefix = "impreza-sdk-go"

// version is set by callers via SetVersion. Default keeps the
// User-Agent useful for unset / dev builds.
var version = "dev"

// SetVersion lets the embedding binary inject its semver so it appears
// in the User-Agent header. Called once at startup; concurrent calls
// are safe but ordering is the caller's problem.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}

// SetUserAgent overrides the User-Agent prefix. Use this from the CLI
// (`impreza-cli-go`), the agent (`impreza-agent`), or any embedder that
// wants its own identity in server-side logs.
func SetUserAgent(name string) {
	if name != "" {
		uaPrefix = name
	}
}

// buildTransport returns the base RoundTripper for a context. Honours
// the context's Tor / Proxy settings:
//
//   - If ctx.Proxy is set, dial through that SOCKS5 URL (any host:port).
//   - Else if ctx.UseTor is true, dial through the default Tor SOCKS port.
//   - Else use http.DefaultTransport.
func buildTransport(ctx config.Context) (http.RoundTripper, error) {
	proxyURL := ctx.Proxy
	if proxyURL == "" && ctx.UseTor {
		proxyURL = tor.DefaultSOCKS
	}
	return tor.Transport(proxyURL)
}

// BaseURL returns the API base URL for a context (its override, or the
// shipped default if unset).
func BaseURL(ctx config.Context) string {
	if ctx.URL != "" {
		return ctx.URL
	}
	return DefaultBaseURL
}
