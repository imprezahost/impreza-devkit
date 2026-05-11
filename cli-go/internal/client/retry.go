package client

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// retryTransport applies exponential backoff retry on transient failures.
// Matches the Python SDK policy: 5 attempts total, base delay 1s,
// doubling each step with ±20% jitter, retry only on:
//
//   - Network errors (transport-layer EOF, connection reset, etc.)
//   - HTTP 429 (Rate Limited) — honours `Retry-After` header if set.
//   - HTTP 502 / 503 / 504 (upstream / gateway timeouts).
//
// Never retries on 4xx other than 429 — those are caller errors and
// retrying would just re-trigger the same response.
type retryTransport struct {
	base http.RoundTripper

	// Test hooks. Zero values mean "use sensible defaults".
	maxAttempts int
	baseDelay   time.Duration
	now         func() time.Time
	sleep       func(time.Duration)
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxAttempts := t.maxAttempts
	if maxAttempts == 0 {
		maxAttempts = 5
	}
	baseDelay := t.baseDelay
	if baseDelay == 0 {
		baseDelay = 1 * time.Second
	}
	sleep := t.sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	// Buffer the body if non-nil so we can replay it across attempts.
	// http.Request.Body is a one-shot Reader by spec; re-using it
	// without buffering breaks retry.
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		bodyBytes = b
	}

	var lastErr error
	var lastResp *http.Response
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Replay the body on every attempt.
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		resp, err := t.base.RoundTrip(req)
		lastResp = resp
		lastErr = err

		if shouldStop(req.Context()) {
			return resp, err
		}

		// Network-level error: retry unless we're out of attempts.
		if err != nil {
			if attempt == maxAttempts-1 {
				return nil, err
			}
			sleep(backoff(attempt, baseDelay, 0))
			continue
		}

		// Transient HTTP status: retry.
		if isRetriableStatus(resp.StatusCode) {
			// Last attempt: return the response intact so the caller
			// can read the body + decode the error envelope.
			if attempt == maxAttempts-1 {
				return resp, nil
			}
			delay := backoff(attempt, baseDelay, parseRetryAfter(resp))
			// Drain + close so the connection can be reused.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			sleep(delay)
			continue
		}

		// 2xx or non-retriable status: return.
		return resp, nil
	}
	return lastResp, lastErr
}

func isRetriableStatus(s int) bool {
	switch s {
	case http.StatusTooManyRequests, // 429
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	default:
		return false
	}
}

// backoff returns the delay before the next attempt. Honours the
// server-supplied Retry-After header when present (capped at 60s to
// keep the CLI responsive — a single command shouldn't pause longer
// than that for retry).
func backoff(attempt int, base time.Duration, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > 60*time.Second {
			return 60 * time.Second
		}
		return retryAfter
	}
	d := base * (1 << uint(attempt)) // 1s, 2s, 4s, 8s, 16s
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	// ±20% jitter to avoid thundering-herd retries.
	jitter := time.Duration(rand.Float64()*0.4*float64(d) - 0.2*float64(d)) //nolint:gosec
	return d + jitter
}

func parseRetryAfter(r *http.Response) time.Duration {
	v := r.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	// Both seconds-since-now and HTTP-date forms are valid per RFC 9110.
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func shouldStop(ctx context.Context) bool {
	return ctx.Err() != nil
}
