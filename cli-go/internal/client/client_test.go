package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/config"
)

// envelopeResponder returns an http.HandlerFunc that emits the canonical
// `{"success": true, "data": <payload>}` envelope.
func envelopeResponder(payload any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    payload,
			"meta":    map[string]string{"request_id": "req_test_001"},
		})
	}
}

// errorResponder returns a handler that emits an envelope error with the
// given HTTP status, code, and message.
func errorResponder(status int, code, msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   map[string]string{"code": code, "message": msg},
			"meta":    map[string]string{"request_id": "req_test_err"},
		})
	}
}

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(config.Context{
		Key:    "imp_test123",
		Secret: "test_secret",
		URL:    srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Shorten retry budget for fast tests.
	c.HTTP.Transport.(*retryTransport).maxAttempts = 2
	c.HTTP.Transport.(*retryTransport).baseDelay = 1 * time.Millisecond
	return c, srv
}

func TestAccountInfoDecodesEnvelope(t *testing.T) {
	c, _ := newTestClient(t, envelopeResponder(map[string]any{
		"id":         1,
		"first_name": "Jane",
		"last_name":  "Doe",
		"email":      "jane@example.com",
		"balance":    12.34,
		"currency":   "USD",
		"status":     "Active",
	}))

	info, err := c.AccountInfo(context.Background())
	if err != nil {
		t.Fatalf("AccountInfo: %v", err)
	}
	if info.ID != 1 || info.FirstName != "Jane" || info.Balance != 12.34 || info.Currency != "USD" {
		t.Errorf("decoded = %+v, want id=1 Jane Doe 12.34 USD", info)
	}
}

func TestAuthHeadersAreInjected(t *testing.T) {
	var gotKey, gotSecret, gotUA string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotSecret = r.Header.Get("X-API-Secret")
		gotUA = r.Header.Get("User-Agent")
		envelopeResponder(map[string]any{"balance": 0.0, "currency": "USD"})(w, r)
	}))

	if _, err := c.AccountInfo(context.Background()); err != nil {
		t.Fatalf("AccountInfo: %v", err)
	}
	if gotKey != "imp_test123" {
		t.Errorf("X-API-Key = %q, want imp_test123", gotKey)
	}
	if gotSecret != "test_secret" {
		t.Errorf("X-API-Secret = %q, want test_secret", gotSecret)
	}
	const prefix = "impreza-cli-go/"
	if gotUA == "" || len(gotUA) <= len(prefix) || gotUA[:len(prefix)] != prefix {
		t.Errorf("User-Agent = %q, want prefix %q", gotUA, prefix)
	}
}

func TestErrorEnvelopeMapsToTypedError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		code       string
		msg        string
		wantType   func(error) bool
		wantStatus int
	}{
		{"401 unauth", 401, "UNAUTHORIZED", "Invalid API key.",
			func(e error) bool { var t *AuthError; return errors.As(e, &t) }, 401},
		{"402 credit", 402, "INSUFFICIENT_CREDIT", "Top up.",
			func(e error) bool { var t *InsufficientCredit; return errors.As(e, &t) }, 402},
		{"403 ip", 403, "IP_NOT_WHITELISTED", "IP not whitelisted.",
			func(e error) bool { var t *IPNotWhitelisted; return errors.As(e, &t) }, 403},
		{"403 other", 403, "FORBIDDEN", "Resource not owned.",
			func(e error) bool { var t *Forbidden; return errors.As(e, &t) }, 403},
		{"404", 404, "NOT_FOUND", "No such resource.",
			func(e error) bool { var t *NotFound; return errors.As(e, &t) }, 404},
		{"409", 409, "CONFLICT", "Already exists.",
			func(e error) bool { var t *Conflict; return errors.As(e, &t) }, 409},
		{"429", 429, "RATE_LIMITED", "Slow down.",
			func(e error) bool { var t *RateLimited; return errors.As(e, &t) }, 429},
		{"500", 500, "INTERNAL_ERROR", "Server died.",
			func(e error) bool { var t *ServerError; return errors.As(e, &t) }, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, errorResponder(tc.status, tc.code, tc.msg))
			_, err := c.AccountInfo(context.Background())
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantType(err) {
				t.Errorf("error type wrong: got %T (%v)", err, err)
			}
			if ae := AsAPIError(err); ae == nil {
				t.Errorf("AsAPIError returned nil")
			} else if ae.Status != tc.wantStatus || ae.Code != tc.code {
				t.Errorf("APIError = %+v, want status=%d code=%s", ae, tc.wantStatus, tc.code)
			}
		})
	}
}

func TestRetryOn429HonoursRetryAfter(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.Header().Set("Retry-After", "0")
			errorResponder(429, "RATE_LIMITED", "Slow down.")(w, r)
			return
		}
		envelopeResponder(map[string]any{"balance": 5.0, "currency": "USD"})(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := New(config.Context{Key: "k", Secret: "s", URL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.HTTP.Transport.(*retryTransport).maxAttempts = 3
	c.HTTP.Transport.(*retryTransport).baseDelay = 1 * time.Millisecond

	if _, err := c.AccountInfo(context.Background()); err != nil {
		t.Fatalf("AccountBalance after retry: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (1 rate-limited + 1 success)", attempts)
	}
}

func TestCatalogTldsFilterIsCommaJoined(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		envelopeResponder(map[string]any{"tlds": []any{}, "total": 0})(w, r)
	}))
	if _, err := c.CatalogTlds(context.Background(), []string{"com", "net", "org"}); err != nil {
		t.Fatalf("CatalogTlds: %v", err)
	}
	if want := "tld=com%2Cnet%2Corg"; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}
}

func TestDomainCheckCommaJoinsArgs(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		envelopeResponder(map[string]any{
			"availability": map[string]bool{
				"example.com": true,
				"example.net": false,
			},
		})(w, r)
	}))
	if _, err := c.DomainCheck(context.Background(), []string{"example.com", "example.net"}); err != nil {
		t.Fatalf("DomainCheck: %v", err)
	}
	if want := "domains=example.com%2Cexample.net"; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}
}
