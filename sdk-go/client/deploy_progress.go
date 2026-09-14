package client

import "context"

const DeploymentProgressProtocol = "deploy-progress-v1"

// DeploymentProgress contains a fixed step identifier, never build logs or credentials.
type DeploymentProgress struct {
	CommandID    string `json:"command_id"`
	ControlToken string `json:"control_token"`
	Step         string `json:"step"`
}
type DeploymentProgressResponse struct {
	CommandID string `json:"command_id"`
	Terminal  bool   `json:"terminal"`
	Status    string `json:"status"`
}

func (c *Client) AgentCommandProgress(ctx context.Context, request DeploymentProgress) (*DeploymentProgressResponse, error) {
	var response DeploymentProgressResponse
	if err := c.Post(ctx, "/v1/agent/command-progress", request, &response); err != nil {
		return nil, err
	}
	return &response, nil
}
