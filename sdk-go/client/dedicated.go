// Dedicated server client surface.
//
// Wraps the public `/dedicated/*` namespace. Operations are gated by
// per-service capabilities: a feature is only available if the service
// advertises it (see DedicatedCapabilities). Calling a capability-gated
// endpoint against a service that doesn't list the capability returns
// NOT_SUPPORTED.
//
// Reinstall is destructive and requires both Confirm:true in the body and
// the X-Impreza-Confirm: WIPE header. The Reinstall method here injects
// the header when Confirm is true so callers can't accidentally omit it.

package client

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// DedicatedSummary is the per-service entry returned by `GET /dedicated`.
type DedicatedSummary struct {
	ServiceID    int      `json:"service_id"`
	Domain       string   `json:"domain"`
	IP           string   `json:"ip"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
}

// DedicatedCapabilities is the response of `GET /dedicated/{id}/capabilities`.
type DedicatedCapabilities struct {
	Capabilities []string `json:"capabilities"`
}

// ReinstallRequest is the body of a dedicated-server reinstall call.
//
// Confirm MUST be true — the wrapper rejects false instead of letting the
// caller learn about it via a 400 from the server.
type ReinstallRequest struct {
	OsID     string `json:"os_id"`
	OsLabel  string `json:"os_label,omitempty"`
	Password string `json:"password"`
	Confirm  bool   `json:"confirm"`
}

// SetFirewallRequest is the body of `PUT /dedicated/{id}/firewall`.
// State and Sensitivity are pointers so that omitting them keeps the
// existing upstream value unchanged.
type SetFirewallRequest struct {
	IP          string  `json:"ip"`
	State       *string `json:"state"`
	Sensitivity *string `json:"sensitivity"`
}

// ── reads ────────────────────────────────────────────────────────────

func (c *Client) DedicatedList(ctx context.Context) ([]DedicatedSummary, error) {
	var out []DedicatedSummary
	if err := c.Get(ctx, "/dedicated", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) DedicatedShow(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	if err := c.Get(ctx, fmt.Sprintf("/dedicated/%d", id), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) DedicatedCapabilities(ctx context.Context, id int) (*DedicatedCapabilities, error) {
	var out DedicatedCapabilities
	if err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/capabilities", id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DedicatedStatus(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/status", id), nil, &out)
	return out, err
}

func (c *Client) DedicatedIps(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/ips", id), nil, &out)
	return out, err
}

func (c *Client) DedicatedOsImages(ctx context.Context, id int) ([]map[string]any, error) {
	var out []map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/os-images", id), nil, &out)
	return out, err
}

func (c *Client) DedicatedKvm(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/kvm", id), nil, &out)
	return out, err
}

func (c *Client) DedicatedFirewall(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/firewall", id), nil, &out)
	return out, err
}

func (c *Client) DedicatedDdosLogs(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/firewall/logs", id), nil, &out)
	return out, err
}

func (c *Client) DedicatedBandwidth(ctx context.Context, id int, kind, scale string) (map[string]any, error) {
	q := url.Values{}
	if kind != "" {
		q.Set("type", kind)
	}
	if scale != "" {
		q.Set("scale", scale)
	}
	var out map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/bandwidth", id), q, &out)
	return out, err
}

func (c *Client) DedicatedVpn(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, fmt.Sprintf("/dedicated/%d/vpn", id), nil, &out)
	return out, err
}

// ── power ────────────────────────────────────────────────────────────

func (c *Client) DedicatedStart(ctx context.Context, id int) error {
	return c.Post(ctx, fmt.Sprintf("/dedicated/%d/start", id), nil, nil)
}

func (c *Client) DedicatedShutdown(ctx context.Context, id int) error {
	return c.Post(ctx, fmt.Sprintf("/dedicated/%d/shutdown", id), nil, nil)
}

func (c *Client) DedicatedReboot(ctx context.Context, id int) error {
	return c.Post(ctx, fmt.Sprintf("/dedicated/%d/reboot", id), nil, nil)
}

// ── rDNS ─────────────────────────────────────────────────────────────

func (c *Client) DedicatedSetRdns(ctx context.Context, id int, ip, hostname string) (map[string]any, error) {
	body := map[string]any{"hostname": hostname}
	var out map[string]any
	err := c.Put(ctx, fmt.Sprintf("/dedicated/%d/ips/%s/rdns", id, ip), body, &out)
	return out, err
}

func (c *Client) DedicatedResetRdns(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Post(ctx, fmt.Sprintf("/dedicated/%d/ips/rdns/reset", id), nil, &out)
	return out, err
}

// ── reinstall (destructive) ──────────────────────────────────────────

// DedicatedReinstall rejects Confirm=false locally and injects the
// X-Impreza-Confirm: WIPE header so callers don't have to remember.
func (c *Client) DedicatedReinstall(ctx context.Context, id int, req ReinstallRequest) (map[string]any, error) {
	if !req.Confirm {
		return nil, errors.New("reinstall wipes all data — set Confirm=true to proceed")
	}
	headers := map[string]string{"X-Impreza-Confirm": "WIPE"}
	var out map[string]any
	err := c.PostWithHeaders(ctx, fmt.Sprintf("/dedicated/%d/reinstall", id), req, headers, &out)
	return out, err
}

// ── KVM ──────────────────────────────────────────────────────────────

func (c *Client) DedicatedEnableKvm(ctx context.Context, id int) (map[string]any, error) {
	var out map[string]any
	err := c.Post(ctx, fmt.Sprintf("/dedicated/%d/kvm/enable", id), nil, &out)
	return out, err
}

func (c *Client) DedicatedDisableKvm(ctx context.Context, id int) error {
	return c.Delete(ctx, fmt.Sprintf("/dedicated/%d/kvm", id), nil)
}

// ── Firewall ─────────────────────────────────────────────────────────

func (c *Client) DedicatedSetFirewall(ctx context.Context, id int, req SetFirewallRequest) (map[string]any, error) {
	var out map[string]any
	err := c.Put(ctx, fmt.Sprintf("/dedicated/%d/firewall", id), req, &out)
	return out, err
}
