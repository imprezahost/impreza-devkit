package client

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is the base error type for every non-2xx response from the
// server. Its fields mirror what the JSON envelope's `error` block
// carries (code + message) plus the request_id from `meta` so users
// can reference it in support tickets.
type APIError struct {
	Status    int    // HTTP status code from the response
	Code      string // Server-side error code (e.g. "UNAUTHORIZED", "INVALID_REQUEST")
	Message   string // Human-readable message from the server
	RequestID string // From the meta.request_id field or X-Request-ID header
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s (code=%s) [request_id=%s]", e.Message, e.Code, e.RequestID)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s (code=%s)", e.Message, e.Code)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// Typed sub-errors so callers can check via errors.As / errors.Is for
// the specific failure mode.
type (
	// AuthError = HTTP 401. Invalid API key or secret.
	AuthError struct{ APIError }

	// IPNotWhitelisted = HTTP 403 with code IP_NOT_WHITELISTED. Caller's
	// IP isn't in the key's whitelist.
	IPNotWhitelisted struct{ APIError }

	// Forbidden = HTTP 403 (any code other than IP_NOT_WHITELISTED).
	Forbidden struct{ APIError }

	// NotFound = HTTP 404.
	NotFound struct{ APIError }

	// InvalidRequest = HTTP 400.
	InvalidRequest struct{ APIError }

	// Conflict = HTTP 409.
	Conflict struct{ APIError }

	// InsufficientCredit = HTTP 402.
	InsufficientCredit struct{ APIError }

	// RateLimited = HTTP 429.
	RateLimited struct{ APIError }

	// ServerError = HTTP 5xx (other than 502/503/504 which retry).
	ServerError struct{ APIError }

	// UpstreamError = HTTP 502/503/504 (after retry budget).
	UpstreamError struct{ APIError }
)

func (e *AuthError) Error() string          { return e.APIError.Error() }
func (e *IPNotWhitelisted) Error() string   { return e.APIError.Error() }
func (e *Forbidden) Error() string          { return e.APIError.Error() }
func (e *NotFound) Error() string           { return e.APIError.Error() }
func (e *InvalidRequest) Error() string     { return e.APIError.Error() }
func (e *Conflict) Error() string           { return e.APIError.Error() }
func (e *InsufficientCredit) Error() string { return e.APIError.Error() }
func (e *RateLimited) Error() string        { return e.APIError.Error() }
func (e *ServerError) Error() string        { return e.APIError.Error() }
func (e *UpstreamError) Error() string      { return e.APIError.Error() }

// Unwrap support so `errors.As(err, &client.APIError{})` works on the
// typed wrappers too.
func (e *AuthError) Unwrap() error          { return &e.APIError }
func (e *IPNotWhitelisted) Unwrap() error   { return &e.APIError }
func (e *Forbidden) Unwrap() error          { return &e.APIError }
func (e *NotFound) Unwrap() error           { return &e.APIError }
func (e *InvalidRequest) Unwrap() error     { return &e.APIError }
func (e *Conflict) Unwrap() error           { return &e.APIError }
func (e *InsufficientCredit) Unwrap() error { return &e.APIError }
func (e *RateLimited) Unwrap() error        { return &e.APIError }
func (e *ServerError) Unwrap() error        { return &e.APIError }
func (e *UpstreamError) Unwrap() error      { return &e.APIError }

// mapStatus converts an HTTP status + envelope error into the right
// typed error. Match the Python SDK's exception hierarchy exactly so
// user code that's familiar with one CLI can reason about the other.
func mapStatus(status int, code, message, requestID string) error {
	if message == "" {
		message = fmt.Sprintf("HTTP %d", status)
	}
	base := APIError{Status: status, Code: code, Message: message, RequestID: requestID}

	switch status {
	case http.StatusBadRequest: // 400
		return &InvalidRequest{base}
	case http.StatusUnauthorized: // 401
		return &AuthError{base}
	case http.StatusPaymentRequired: // 402
		return &InsufficientCredit{base}
	case http.StatusForbidden: // 403
		if code == "IP_NOT_WHITELISTED" {
			return &IPNotWhitelisted{base}
		}
		return &Forbidden{base}
	case http.StatusNotFound: // 404
		return &NotFound{base}
	case http.StatusConflict: // 409
		return &Conflict{base}
	case http.StatusTooManyRequests: // 429
		return &RateLimited{base}
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 502/503/504
		return &UpstreamError{base}
	default:
		if status >= 500 {
			return &ServerError{base}
		}
		return &base
	}
}

// AsAPIError extracts an *APIError from err if present. Returns nil if
// err isn't an API error (e.g. a network failure).
func AsAPIError(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}
