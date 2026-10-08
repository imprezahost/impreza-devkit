package client

import (
	"context"
	"time"
)

// HostInventoryProtocol carries allowlisted facts only. No configuration blobs,
// environment, logs, addresses or provider identifiers are accepted from guests.
const HostInventoryProtocol = "host-inventory-v1"

type InventoryFact struct {
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	ValidUntil time.Time `json:"valid_until"`
	State      string    `json:"state"`
	Reason     string    `json:"reason"`
	Value      any       `json:"value"`
}
type HostInventory struct {
	Protocol   string                   `json:"protocol"`
	ObservedAt time.Time                `json:"observed_at"`
	Facts      map[string]InventoryFact `json:"facts"`
}
type InventoryRequest struct {
	Due       bool   `json:"due"`
	RequestID string `json:"request_id"`
}

// AgentHeartbeat accepts optional inventory demand in the heartbeat response.
// A legacy 204 remains valid and cannot trigger any collection.
func (c *Client) AgentHeartbeat(ctx context.Context, r AgentReport) (InventoryRequest, error) {
	var demand InventoryRequest
	err := c.Post(ctx, "/v1/agent/report", r, &demand)
	return demand, err
}
