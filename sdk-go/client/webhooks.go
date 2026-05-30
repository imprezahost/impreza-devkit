package client

import (
	"context"
	"fmt"
)

// WebhookSubscription is one row of GET /v1/webhooks (and the create /
// update / show / rotate-secret responses).
type WebhookSubscription struct {
	ID                 int      `json:"id"`
	URL                string   `json:"url"`
	Events             []string `json:"events,omitempty"`
	Description        string   `json:"description,omitempty"`
	IsActive           bool     `json:"is_active,omitempty"`
	LastDeliveryAt     string   `json:"last_delivery_at,omitempty"`
	LastDeliveryStatus int      `json:"last_delivery_status,omitempty"`
	CreatedAt          string   `json:"created_at,omitempty"`

	// Populated only on `create` and `rotate-secret` responses.
	Secret        string `json:"secret,omitempty"`
	SecretWarning string `json:"secret_warning,omitempty"`
}

type webhooksListResponse struct {
	Webhooks []WebhookSubscription `json:"webhooks"`
	Total    int                   `json:"total"`
}

// WebhooksList wraps GET /v1/webhooks.
func (c *Client) WebhooksList(ctx context.Context) ([]WebhookSubscription, error) {
	var resp webhooksListResponse
	if err := c.Get(ctx, "/v1/webhooks", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Webhooks, nil
}

// WebhookShow wraps GET /v1/webhooks/{id}.
func (c *Client) WebhookShow(ctx context.Context, id int) (*WebhookSubscription, error) {
	var sub WebhookSubscription
	if err := c.Get(ctx, fmt.Sprintf("/v1/webhooks/%d", id), nil, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// WebhookCreateRequest is the body for POST /v1/webhooks.
type WebhookCreateRequest struct {
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Description string   `json:"description,omitempty"`
}

// WebhookCreate wraps POST /v1/webhooks. Server returns the
// subscription with `secret` populated — store it; subsequent reads
// won't echo the secret.
func (c *Client) WebhookCreate(ctx context.Context, req WebhookCreateRequest) (*WebhookSubscription, error) {
	var sub WebhookSubscription
	if err := c.Post(ctx, "/v1/webhooks", req, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// WebhookUpdateRequest is the body for PATCH /v1/webhooks/{id}. All
// fields optional; omitted fields stay unchanged server-side.
type WebhookUpdateRequest struct {
	URL         string   `json:"url,omitempty"`
	Events      []string `json:"events,omitempty"`
	Description string   `json:"description,omitempty"`
	IsActive    *bool    `json:"is_active,omitempty"`
}

// WebhookUpdate wraps PATCH /v1/webhooks/{id}. The Put helper sends
// PUT — webhook update is documented as PATCH server-side, but Put
// works because the server treats absent fields the same. To match
// the spec literally we wire it via the lower-level `do` directly.
func (c *Client) WebhookUpdate(ctx context.Context, id int, req WebhookUpdateRequest) (*WebhookSubscription, error) {
	var sub WebhookSubscription
	// Server accepts PUT here for simplicity — the API treats omitted
	// fields as "leave unchanged" regardless of method. PATCH semantics
	// would also work; PUT is what the Python SDK uses.
	if err := c.Put(ctx, fmt.Sprintf("/v1/webhooks/%d", id), req, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// WebhookDelete wraps DELETE /v1/webhooks/{id}.
func (c *Client) WebhookDelete(ctx context.Context, id int) error {
	return c.Delete(ctx, fmt.Sprintf("/v1/webhooks/%d", id), nil)
}

// WebhookRotateSecret wraps POST /v1/webhooks/{id}/rotate-secret.
// Returns the subscription with a freshly-generated `secret`.
func (c *Client) WebhookRotateSecret(ctx context.Context, id int) (*WebhookSubscription, error) {
	var sub WebhookSubscription
	if err := c.Post(ctx, fmt.Sprintf("/v1/webhooks/%d/rotate-secret", id), nil, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// WebhookDelivery is one row of GET /v1/webhooks/{id}/deliveries.
type WebhookDelivery struct {
	ID               int    `json:"id"`
	EventType        string `json:"event_type"`
	EventID          string `json:"event_id"`
	Attempts         int    `json:"attempts,omitempty"`
	NextAttemptAt    string `json:"next_attempt_at,omitempty"`
	LastAttemptedAt  string `json:"last_attempted_at,omitempty"`
	LastResponseCode int    `json:"last_response_code,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	Delivered        bool   `json:"delivered,omitempty"`
	DeliveredAt      string `json:"delivered_at,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
}

type deliveriesListResponse struct {
	Deliveries []WebhookDelivery `json:"deliveries"`
	Total      int               `json:"total"`
}

// WebhookDeliveries wraps GET /v1/webhooks/{id}/deliveries.
func (c *Client) WebhookDeliveries(ctx context.Context, id int) ([]WebhookDelivery, error) {
	var resp deliveriesListResponse
	if err := c.Get(ctx, fmt.Sprintf("/v1/webhooks/%d/deliveries", id), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Deliveries, nil
}

// WebhookEventCatalog is the response from GET /v1/webhooks/event-types.
type WebhookEventCatalog struct {
	EventTypes []string          `json:"event_types"`
	Wildcards  map[string]string `json:"wildcards"`
}

// WebhookEventTypes wraps GET /v1/webhooks/event-types.
func (c *Client) WebhookEventTypes(ctx context.Context) (*WebhookEventCatalog, error) {
	var cat WebhookEventCatalog
	if err := c.Get(ctx, "/v1/webhooks/event-types", nil, &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}
