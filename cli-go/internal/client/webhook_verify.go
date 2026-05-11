package client

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrWebhookSignatureMismatch is returned by VerifySignature when the
// supplied X-Impreza-Signature header doesn't match the HMAC-SHA256
// of the body keyed by the subscription's secret.
var ErrWebhookSignatureMismatch = errors.New("webhook signature mismatch")

// WebhookEvent is the canonical decoded shape of a delivered event.
// Matches the AsyncAPI EventEnvelope (id, type, created_at, data).
type WebhookEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	CreatedAt string         `json:"created_at"`
	Data      map[string]any `json:"data,omitempty"`
}

// VerifySignature is the canonical receiver-side helper for webhook
// deliveries. It performs the HMAC check + JSON decode in one call.
//
// Inputs:
//   - body:      raw request body bytes (DO NOT re-serialize JSON
//                first — signature is over the bytes the server sent).
//   - signature: the hex-encoded HMAC from the X-Impreza-Signature
//                request header.
//   - secret:    the subscription's secret (from the create response).
//
// Returns:
//   - The decoded WebhookEvent on success.
//   - ErrWebhookSignatureMismatch (wrapped) if the HMAC check fails.
//   - A wrapped JSON error if the body isn't valid JSON.
//
// Constant-time HMAC comparison via hmac.Equal — critical for security.
func VerifySignature(body []byte, signature, secret string) (*WebhookEvent, error) {
	if signature == "" {
		return nil, fmt.Errorf("%w: empty signature", ErrWebhookSignatureMismatch)
	}
	if secret == "" {
		return nil, errors.New("VerifySignature: secret is required")
	}

	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return nil, fmt.Errorf("%w: not valid hex", ErrWebhookSignatureMismatch)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)

	if !hmac.Equal(sigBytes, want) {
		return nil, ErrWebhookSignatureMismatch
	}

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("decode webhook body: %w", err)
	}
	return &event, nil
}
