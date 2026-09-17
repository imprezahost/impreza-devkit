package client

import (
	"context"
	"errors"
	"net/url"
	"regexp"
)

const ServiceBindingProtocol = "postgres-service-binding-v1"
const ServiceBindingRetirementProtocol = "postgres-service-binding-retire-v1"

// Generation protocols keep database ownership separate from the managed login.
const ServiceBindingGenerationProtocol = "postgres-service-binding-v2"
const ServiceBindingGenerationRetirementProtocol = "postgres-service-binding-retire-v2"

// ServiceBindingRotationProtocol replaces one verified generation login with a
// distinct revision. The serving credential never changes before the replacement
// consumer is healthy, and a rotation never retires the login it serves.
const ServiceBindingRotationProtocol = "postgres-service-binding-rotation-v1"

// References contain no credentials. The agent must authorize them for the current command.
type ServiceBindingRef struct {
	BindingID            string `json:"binding_id"`
	ProviderDeploymentID string `json:"provider_deployment_id"`
	Variable             string `json:"variable"`
	Revision             string `json:"revision"`
}

type ServiceBindingCredential struct {
	ServiceBindingRef
	Username  string `json:"username"`
	Database  string `json:"database"`
	Password  string `json:"password"`
	AdminUser string `json:"admin_user"`
}

// ServiceBindingRetirement authorizes disabling one verified dedicated login.
// It deliberately carries no password. The database and its data are retained.
type ServiceBindingRetirement struct {
	ServiceBindingRef
	Username  string `json:"username"`
	Database  string `json:"database"`
	AdminUser string `json:"admin_user"`
}

type ServiceBindingCredentials struct {
	Protocol string                     `json:"protocol"`
	Bindings []ServiceBindingCredential `json:"bindings"`
}

type ServiceBindingRetirements struct {
	Protocol    string                     `json:"protocol"`
	Retirements []ServiceBindingRetirement `json:"retirements"`
}

type ServiceBindingRetirementResult struct {
	ServiceBindingRef
	Status       string `json:"status"` // retired | pending
	DataRetained bool   `json:"data_retained"`
}

// ServiceBindingRotationIntent is the secret-free manifest intent. Candidate
// differs from previous only in revision; service_bindings carries the serving
// reference (candidate on rotate, previous on abandon).
type ServiceBindingRotationIntent struct {
	RotationID string            `json:"rotation_id"`
	Mode       string            `json:"mode"` // rotate | abandon
	Previous   ServiceBindingRef `json:"previous"`
	Candidate  ServiceBindingRef `json:"candidate"`
}

// ServiceBindingRotationResult is the durable outcome of a rotation command.
// It is reported only on a successful replacement; failed startups carry no
// rotation outcome and retain both credentials server-side.
type ServiceBindingRotationResult struct {
	BindingID         string `json:"binding_id"`
	CandidateRevision string `json:"candidate_revision"`
	DataRetained      bool   `json:"data_retained"`
	PreviousRevision  string `json:"previous_revision"`
	RotationID        string `json:"rotation_id"`
	Status            string `json:"status"` // completed | cleanup_pending | abandoned | abandon_pending
}

func (c *Client) AgentServiceBindingRetirements(ctx context.Context, deploymentID, commandID, controlToken string) (*ServiceBindingRetirements, error) {
	if !regexp.MustCompile(`^dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})$`).MatchString(deploymentID) {
		return nil, errors.New("invalid deployment identity")
	}
	var result ServiceBindingRetirements
	err := c.Post(ctx, "/v1/agent/service-binding-retirements/"+url.PathEscape(deploymentID), map[string]string{"command_id": commandID, "control_token": controlToken}, &result)
	return &result, err
}

func (c *Client) AgentServiceBindings(ctx context.Context, deploymentID, commandID, controlToken string) (*ServiceBindingCredentials, error) {
	if !regexp.MustCompile(`^dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})$`).MatchString(deploymentID) {
		return nil, errors.New("invalid deployment identity")
	}
	var result ServiceBindingCredentials
	err := c.Post(ctx, "/v1/agent/service-bindings/"+url.PathEscape(deploymentID), map[string]string{"command_id": commandID, "control_token": controlToken}, &result)
	return &result, err
}
