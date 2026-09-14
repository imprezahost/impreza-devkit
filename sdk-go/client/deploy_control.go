package client

import "context"

// DeploymentControl is an authenticated preparation checkpoint. The token is
// issued for this claimed command only, and must never be shown to customers.
type DeploymentControl struct {
	CommandID    string `json:"command_id"`
	ControlToken string `json:"control_token"`
	Phase        string `json:"phase"`
}
type DeploymentControlResponse struct {
	CommandID       string `json:"command_id"`
	CancelRequested bool   `json:"cancel_requested"`
	Phase           string `json:"phase"`
}

func (c *Client) AgentCommandControl(ctx context.Context, request DeploymentControl) (*DeploymentControlResponse, error) {
	var response DeploymentControlResponse
	if err := c.Post(ctx, "/v1/agent/command-control", request, &response); err != nil {
		return nil, err
	}
	return &response, nil
}
