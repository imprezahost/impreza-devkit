package client

// Tor v3 restricted discovery (client authorization) —
// /v1/platform/deployments/{id}/onion/clients. A restricted hidden
// service is reachable ONLY by Tor clients presenting an authorized
// x25519 key; these helpers manage that authorization list.
//
// Key custody: the client's private key never leaves the customer's
// own Tor client when they supply a pubkey. With Generate=true the
// server mints the keypair and returns the private key ONCE — it is
// never stored and cannot be retrieved later.

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// OnionAuthClientEntry is one authorized client of a restricted onion
// service, as returned by the list endpoint. (The agent-realm
// OnionAuthClient in agent.go is the name+pubkey wire type for the
// desired-state sync — different shape, different audience.)
type OnionAuthClientEntry struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// OnionAuthClientList is the data payload of GET .../onion/clients.
type OnionAuthClientList struct {
	Restricted bool                   `json:"restricted"`
	Clients    []OnionAuthClientEntry `json:"clients"`
}

// OnionAuthAddRequest is the body of POST .../onion/clients. Supply
// either Pubkey (the client's own x25519 public key) or Generate=true
// (the server mints the keypair) — never both.
type OnionAuthAddRequest struct {
	Name     string `json:"name"`
	Pubkey   string `json:"pubkey,omitempty"`
	Generate bool   `json:"generate,omitempty"`
}

// OnionAuthAddResult is the data payload of POST .../onion/clients.
// PrivateKey is set ONLY when the request had Generate=true; it is
// shown once and never stored server-side.
type OnionAuthAddResult struct {
	Name       string `json:"name"`
	Pubkey     string `json:"pubkey"`
	PrivateKey string `json:"private_key,omitempty"`
}

// PlatformOnionAuthList wraps GET /v1/platform/deployments/{id}/onion/clients.
func (c *Client) PlatformOnionAuthList(ctx context.Context, id string) (*OnionAuthClientList, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	var out OnionAuthClientList
	if err := c.Get(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/clients", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformOnionAuthAdd wraps POST /v1/platform/deployments/{id}/onion/clients.
// Exactly one of req.Pubkey / req.Generate must be set.
func (c *Client) PlatformOnionAuthAdd(ctx context.Context, id string, req OnionAuthAddRequest) (*OnionAuthAddResult, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	if req.Name == "" {
		return nil, fmt.Errorf("client name is required")
	}
	if req.Generate && req.Pubkey != "" {
		return nil, fmt.Errorf("pass either pubkey or generate, not both")
	}
	if !req.Generate && req.Pubkey == "" {
		return nil, fmt.Errorf("pubkey is required unless generate is set")
	}
	var out OnionAuthAddResult
	if err := c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/clients", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformOnionAuthRevoke wraps DELETE /v1/platform/deployments/{id}/onion/clients/{name}.
func (c *Client) PlatformOnionAuthRevoke(ctx context.Context, id, name string) error {
	if id == "" {
		return fmt.Errorf("deployment id is required")
	}
	if name == "" {
		return fmt.Errorf("client name is required")
	}
	return c.Delete(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/clients/"+url.PathEscape(name), nil)
}
