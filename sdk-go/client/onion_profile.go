package client

// Tor v3 hardening profiles —
// POST /v1/platform/deployments/{id}/onion/profile changes the tier of an
// EXISTING hidden service; deploy bodies accept the same `onion_profile`
// field at creation time. The .onion address never changes.
//
// Tiers: standard = intro-point rate limiting (the safe upstream default);
// hardened = tighter limits + max streams; max = + experimental
// proof-of-work — one layer among several, never a guaranteed DDoS
// protection, and the change may be refused when the host's Tor lacks the
// PoW module.

import (
	"context"
	"fmt"
	"net/url"
)

// Onion profile tiers accepted by deploy bodies and the profile endpoint.
const (
	OnionProfileStandard = "standard"
	OnionProfileHardened = "hardened"
	OnionProfileMax      = "max"
)

// ValidOnionProfileTier reports whether p is a tier the platform knows
// how to render.
func ValidOnionProfileTier(p string) bool {
	switch p {
	case OnionProfileStandard, OnionProfileHardened, OnionProfileMax:
		return true
	}
	return false
}

// OnionProfileResponse is the data payload of POST .../onion/profile
// (202): the enqueued command, the tier that will be applied, and a
// human note from the server.
type OnionProfileResponse struct {
	CommandID string `json:"command_id"`
	Profile   string `json:"profile"`
	Note      string `json:"note"`
}

// PlatformSetOnionProfile wraps POST /v1/platform/deployments/{id}/onion/profile.
// 404 when the deployment has no onion service, 422 when the agent lacks
// onion-profile-v1.
func (c *Client) PlatformSetOnionProfile(ctx context.Context, id, profile string) (*OnionProfileResponse, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	if !ValidOnionProfileTier(profile) {
		return nil, fmt.Errorf("profile must be one of: %s, %s, %s", OnionProfileStandard, OnionProfileHardened, OnionProfileMax)
	}
	var out OnionProfileResponse
	if err := c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/onion/profile", map[string]string{"profile": profile}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
