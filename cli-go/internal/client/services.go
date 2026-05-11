package client

import (
	"context"
	"fmt"
)

// CancelType is the accepted set of values for ServiceCancel.cancelType.
var validCancelTypes = map[string]bool{
	"Immediate":             true,
	"End of Billing Period": true,
}

// ServiceCancel submits an AddCancelRequest for any service (VPS,
// hosting, email, domain). Mirrors the SDK semantics: staff approves
// the actual termination, customer never terminates services directly.
//
// cancelType must be "Immediate" or "End of Billing Period".
// reason is optional free-text the customer supplies.
func (c *Client) ServiceCancel(ctx context.Context, serviceID int, cancelType, reason string) error {
	if !validCancelTypes[cancelType] {
		return fmt.Errorf("ServiceCancel: type must be \"Immediate\" or \"End of Billing Period\" (got %q)", cancelType)
	}
	body := map[string]string{"type": cancelType}
	if reason != "" {
		body["reason"] = reason
	}
	path := fmt.Sprintf("/v1/services/%d/cancel", serviceID)
	return c.Post(ctx, path, body, nil)
}
