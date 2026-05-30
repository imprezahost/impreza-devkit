// Cloud VPS sub-resources: images, rescue, iso, rdns, ssh-keys + the
// inline verbs (vnc, vnc-password, resize, boot-order, ipv6).
// Mirrors the Python SDK's vps_cloud.py resource layer.
//
// Cloud uses two URL roots:
//   /v1/vps/cloud/<resource>           — account-scoped (images list,
//                                        rdns by IP, ssh-keys list)
//   /v1/vps/cloud/{vm_id}/<resource>   — per-VM (rescue, iso, vnc,
//                                        boot-order, ipv6, etc.)
package client

import (
	"context"
	"fmt"
	"net/url"
)

// ── Images ────────────────────────────────────────────────────────

// Image is one row of GET /v1/vps/cloud/images.
type Image struct {
	ID          any    `json:"id"` // server may emit string or int
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
	SizeMB      int    `json:"size_mb,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	VmID        any    `json:"vm_id,omitempty"`
}

type imagesListResponse struct {
	Images []Image `json:"images"`
	Total  int     `json:"total"`
}

// CloudImagesList wraps GET /v1/vps/cloud/images. Account-scoped — the
// image catalog is per-account, not per-VM.
func (c *Client) CloudImagesList(ctx context.Context) ([]Image, error) {
	var resp imagesListResponse
	if err := c.Get(ctx, "/v1/vps/cloud/images", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Images, nil
}

// CloudImageCreate wraps POST /v1/vps/cloud/{vm_id}/images. Snapshots
// the bound VM's current state into a saved image. The server echoes
// `{result, response: {message}}` — no id is returned. The image
// shows up in CloudImagesList() once the upstream queue finishes.
func (c *Client) CloudImageCreate(ctx context.Context, vmID int) error {
	path := fmt.Sprintf("/v1/vps/cloud/%d/images", vmID)
	return c.Post(ctx, path, nil, nil)
}

// CloudImageRestore wraps POST /v1/vps/cloud/{vm_id}/images/{image_id}/restore.
// Restores the bound VM from a previously-saved image. Destructive —
// the current VM state is overwritten.
func (c *Client) CloudImageRestore(ctx context.Context, vmID int, imageID string) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/cloud/%d/images/%s/restore", vmID, url.PathEscape(imageID))
	if err := c.Post(ctx, path, nil, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// CloudImageDelete wraps DELETE /v1/vps/cloud/images/{image_id}.
// Account-scoped (no vm_id in path).
func (c *Client) CloudImageDelete(ctx context.Context, imageID string) error {
	path := fmt.Sprintf("/v1/vps/cloud/images/%s", url.PathEscape(imageID))
	return c.Delete(ctx, path, nil)
}

// ── Rescue ────────────────────────────────────────────────────────

// CloudRescueEnable wraps POST /v1/vps/cloud/{vm_id}/rescue. Enables
// rescue mode with the supplied password. VM reboot required to enter
// rescue.
func (c *Client) CloudRescueEnable(ctx context.Context, vmID int, password string) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/cloud/%d/rescue", vmID)
	if err := c.Post(ctx, path, map[string]string{"password": password}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// CloudRescueDisable wraps DELETE /v1/vps/cloud/{vm_id}/rescue.
func (c *Client) CloudRescueDisable(ctx context.Context, vmID int) error {
	path := fmt.Sprintf("/v1/vps/cloud/%d/rescue", vmID)
	return c.Delete(ctx, path, nil)
}

// ── ISO ───────────────────────────────────────────────────────────

// CloudIsoMount wraps POST /v1/vps/cloud/{vm_id}/iso/mount. The
// `iso` identifier comes from the Cloud backend's available ISO list
// (which varies per location).
func (c *Client) CloudIsoMount(ctx context.Context, vmID int, iso string) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/cloud/%d/iso/mount", vmID)
	if err := c.Post(ctx, path, map[string]string{"iso": iso}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// CloudIsoUnmount wraps DELETE /v1/vps/cloud/{vm_id}/iso.
func (c *Client) CloudIsoUnmount(ctx context.Context, vmID int) error {
	path := fmt.Sprintf("/v1/vps/cloud/%d/iso", vmID)
	return c.Delete(ctx, path, nil)
}

// ── rDNS ──────────────────────────────────────────────────────────

// CloudRdnsGet wraps GET /v1/vps/cloud/rdns/{ip}.
//
// **Known issue (carried from Phase 3.5):** the application firewall
// intercepts dotted-IPv4 path segments here and returns a maintenance
// HTML page. Until the WAF rule lands, GET will likely 404 or return
// an HTML body the JSON decoder rejects. Workaround: use the
// Impreza Account panel.
func (c *Client) CloudRdnsGet(ctx context.Context, ip string) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/cloud/rdns/%s", url.PathEscape(ip))
	if err := c.Get(ctx, path, nil, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// CloudRdnsSet wraps PUT /v1/vps/cloud/rdns/{ip}.
//
// Server expects `domain` (not `hostname`) on the body — the Python
// SDK currently uses `hostname` which the live server rejects with
// `INVALID_REQUEST: Field 'domain' is required`. Caught during the
// Phase 7.4 live smoke. We use `domain` to match what the live API
// actually accepts; Python SDK to be updated separately.
func (c *Client) CloudRdnsSet(ctx context.Context, ip, hostname string) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/cloud/rdns/%s", url.PathEscape(ip))
	if err := c.Put(ctx, path, map[string]string{"domain": hostname}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// CloudRdnsDelete wraps DELETE /v1/vps/cloud/rdns/{ip}.
func (c *Client) CloudRdnsDelete(ctx context.Context, ip string) error {
	path := fmt.Sprintf("/v1/vps/cloud/rdns/%s", url.PathEscape(ip))
	return c.Delete(ctx, path, nil)
}

// ── SSH keys ──────────────────────────────────────────────────────

// SshKey is one row of GET /v1/vps/cloud/ssh-keys. Account-level
// records (the Cloud backend treats SSH keys as account-scoped,
// reusable across VMs).
type SshKey struct {
	ID          any    `json:"id"`
	Name        string `json:"name,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

type sshKeysListResponse struct {
	SshKeys []SshKey `json:"ssh_keys"`
	Total   int      `json:"total"`
}

// CloudSshKeysList wraps GET /v1/vps/cloud/ssh-keys.
func (c *Client) CloudSshKeysList(ctx context.Context) ([]SshKey, error) {
	var resp sshKeysListResponse
	if err := c.Get(ctx, "/v1/vps/cloud/ssh-keys", nil, &resp); err != nil {
		return nil, err
	}
	return resp.SshKeys, nil
}

// CloudSshKeysAssign wraps POST /v1/vps/cloud/{vm_id}/ssh-keys. Body
// is `{ssh_keys: [<id-or-name>...]}` — server accepts either ids or
// labels.
func (c *Client) CloudSshKeysAssign(ctx context.Context, vmID int, keys []string) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/cloud/%d/ssh-keys", vmID)
	if err := c.Post(ctx, path, map[string]any{"ssh_keys": keys}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// ── Inline Cloud verbs (per-VM) ───────────────────────────────────

// VncCredentials is the response from GET /v1/vps/cloud/{vm_id}/vnc.
type VncCredentials struct {
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Password string `json:"password"`
}

// CloudVnc wraps GET /v1/vps/cloud/{vm_id}/vnc.
func (c *Client) CloudVnc(ctx context.Context, vmID int) (*VncCredentials, error) {
	var v VncCredentials
	path := fmt.Sprintf("/v1/vps/cloud/%d/vnc", vmID)
	if err := c.Get(ctx, path, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CloudVncPassword wraps PUT /v1/vps/cloud/{vm_id}/vnc-password.
func (c *Client) CloudVncPassword(ctx context.Context, vmID int, password string) error {
	path := fmt.Sprintf("/v1/vps/cloud/%d/vnc-password", vmID)
	return c.Put(ctx, path, map[string]string{"password": password}, nil)
}

// CloudResize wraps POST /v1/vps/cloud/{vm_id}/resize. Reboot required
// to apply the new instance size.
func (c *Client) CloudResize(ctx context.Context, vmID int, instanceSize string) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/cloud/%d/resize", vmID)
	if err := c.Post(ctx, path, map[string]string{"instance_size": instanceSize}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// CloudBootOrder wraps PUT /v1/vps/cloud/{vm_id}/boot-order. Accepts
// "cda" (disk → cdrom → network) or "dca" (cdrom → disk → network).
func (c *Client) CloudBootOrder(ctx context.Context, vmID int, order string) error {
	if order != "cda" && order != "dca" {
		return fmt.Errorf("boot order must be 'cda' or 'dca' (got %q)", order)
	}
	path := fmt.Sprintf("/v1/vps/cloud/%d/boot-order", vmID)
	return c.Put(ctx, path, map[string]string{"bootorder": order}, nil)
}

// CloudIpv6Enable wraps POST /v1/vps/cloud/{vm_id}/ipv6.
func (c *Client) CloudIpv6Enable(ctx context.Context, vmID int) error {
	path := fmt.Sprintf("/v1/vps/cloud/%d/ipv6", vmID)
	return c.Post(ctx, path, nil, nil)
}
