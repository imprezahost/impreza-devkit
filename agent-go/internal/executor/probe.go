package executor

// HTTPS probe — runs at the end of a successful `compose up` to confirm
// the customer-facing URL is actually serving a valid TLS cert before
// the agent reports `running` to the control plane.
//
// Why this exists (Phase 9.23, 2026-05-26):
//   `compose up` returning 0 only proves the container started — it
//   says nothing about whether Caddy already obtained the LE cert,
//   whether the upstream container is listening on its port, or
//   whether the route is wired correctly. Customers who clicked the
//   URL the second the panel flipped to 'running' got
//   ERR_SSL_PROTOCOL_ERROR (cert mid-issuance) or 502/504 (upstream
//   not ready). Reads as 'install broken' even though everything is
//   on the right path; you just had to wait 30 more seconds.
//
//   This probe makes the agent BLOCK for up to 90s on the deploy
//   command — looping HEAD requests against the route's HTTPS endpoint
//   every 2s — until one returns a status code that proves the TLS
//   handshake succeeded AND the upstream is alive. THEN we report
//   running. The customer's first click works.
//
// Behavior matrix on response status:
//   200-399  : TLS handshake OK + app responding → PASS
//   401, 403 : TLS handshake OK + app responding (just auth-walled) → PASS
//   502, 503,
//   504      : TLS handshake OK + upstream not ready → KEEP WAITING
//   anything else (incl. transport errors / cert errors) → KEEP WAITING
//
// Fail-open: if the 90s budget expires without a PASS, we LOG a warning
// and proceed to report running anyway. Reasons we'd legitimately time
// out without an actual problem:
//   - BYO domain whose DNS A record hasn't propagated to the agent's
//     public IP yet — the probe (from the agent itself) goes through
//     the host's DNS resolver which may also not see the new record.
//     The cert WILL issue eventually via DNS-01 + the URL WILL work
//     once propagation completes; we just can't confirm it within
//     the 90s budget.
//   - Apps with very slow startup (Synapse cold start, large Nextcloud
//     init) where the upstream isn't reachable through Caddy yet.
//
//   The install-success flash already warns the customer that the
//   first click may transiently fail, so a fail-open is graceful
//   degradation rather than a regression.

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// probeBudget is the wall-clock ceiling for the probe loop. 90s
// comfortably covers a fresh DNS-01 challenge + Caddy install + a
// small container's first HTTPS response.
const probeBudget = 90 * time.Second

// probeInterval is how often we retry between attempts. 2s is fast
// enough to catch the cert flip within a heartbeat but slow enough
// to not blast Caddy's ACME loop with handshake attempts during
// issuance.
const probeInterval = 2 * time.Second

// probeTimeout is the per-request timeout. Generous to absorb a slow
// cold-start TLS handshake but tight enough that a stuck connection
// doesn't eat the whole budget.
const probeTimeout = 5 * time.Second

// ProbeHTTPS does the actual loop. Returns true if any retry sees a
// status code that proves the TLS handshake + reverse_proxy upstream
// are both OK. Returns false if the budget expires without success.
//
// The host argument is the bare clearnet hostname (e.g.
// "vault-abc.imprezaapps.com") — we tack on "https://" + "/" ourselves.
// Callers pass "" to no-op (e.g. onion-only deploys with no clearnet
// route).
func ProbeHTTPS(ctx context.Context, host string, log *slog.Logger) bool {
	if host == "" {
		return false
	}
	url := "https://" + host + "/"

	// Build a one-off http.Client. We do NOT reuse it across deploys
	// because the TLS cache shouldn't survive between probes — a fresh
	// run should re-verify the cert as the customer's browser would.
	client := &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			// Verify the cert chain — the whole point of this probe
			// is to catch the window where the cert ISN'T valid yet.
			// InsecureSkipVerify here would defeat the purpose.
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
			},
			// Don't reuse connections across attempts — a stale
			// connection to a half-restarted Caddy could mask a real
			// failure.
			DisableKeepAlives: true,
		},
	}

	deadline := time.Now().Add(probeBudget)
	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		if ctx.Err() != nil {
			log.Warn("https probe: parent context cancelled mid-probe", "host", host, "attempts", attempt)
			return false
		}
		// Use a per-attempt context with its own timeout so a stuck
		// handshake can't blow past the global budget.
		reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
		if err != nil {
			cancel()
			return false // misformed url — propagate as fail
		}
		req.Header.Set("User-Agent", "impreza-agent-probe/1.0")
		resp, err := client.Do(req)
		cancel()
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			if probeStatusPasses(resp.StatusCode) {
				log.Info("https probe: passed",
					"host", host,
					"status", resp.StatusCode,
					"attempts", attempt,
					"elapsed", time.Since(deadline.Add(-probeBudget)).Round(time.Millisecond))
				return true
			}
			log.Debug("https probe: still waiting",
				"host", host,
				"status", resp.StatusCode,
				"attempt", attempt)
		} else if err != nil {
			log.Debug("https probe: transport error",
				"host", host,
				"err", err,
				"attempt", attempt)
		}

		// Sleep before next attempt, but bail early if ctx cancels.
		select {
		case <-ctx.Done():
			log.Warn("https probe: parent context cancelled between attempts", "host", host, "attempts", attempt)
			return false
		case <-time.After(probeInterval):
		}
	}
	log.Warn(fmt.Sprintf("https probe: %s did not respond OK within %s; proceeding anyway", host, probeBudget),
		"attempts", attempt)
	return false
}

// probeStatusPasses encodes the response-code matrix from the file
// header docblock. Any positive code that PROVES the TLS handshake
// succeeded AND something is answering on the upstream is a pass.
func probeStatusPasses(code int) bool {
	switch {
	case code >= 200 && code < 400:
		// 2xx: app responded OK. 3xx: redirect (e.g. to /login) —
		// upstream is alive.
		return true
	case code == 401 || code == 403:
		// Auth-walled but the TLS handshake worked + upstream
		// responded. From the cert / network perspective, ready.
		return true
	}
	// 5xx, 4xx other than 401/403, transport errors, etc. → keep
	// waiting.
	return false
}
