package client

import (
	"context"
	"errors"
	"fmt"
)

// powerPath maps a normalised action verb to the backend's URL
// segment. Proxmox uses the verb names directly; Cloud renames
// start→boot and stop→poweroff (the actual REST paths upstream).
func powerPath(backend, action string) (string, error) {
	switch backend {
	case "proxmox":
		switch action {
		case "start", "stop", "reboot", "shutdown":
			return action, nil
		}
	case "cloud":
		switch action {
		case "start":
			return "boot", nil
		case "stop":
			return "poweroff", nil
		case "reboot":
			return "reboot", nil
		case "shutdown":
			return "shutdown", nil
		}
	}
	return "", fmt.Errorf("unsupported power action %q on backend %q", action, backend)
}

// vpsPower dispatches a power action to /vps/{backend}/{id}/{path}.
// Resolves the backend via AccountServiceShow first (one extra GET).
func (c *Client) vpsPower(ctx context.Context, serviceID int, action string) error {
	svc, err := c.AccountServiceShow(ctx, serviceID)
	if err != nil {
		return err
	}
	if svc.VpsBackend == "" {
		return fmt.Errorf("service %d is not a VPS (backend not set)", serviceID)
	}
	seg, err := powerPath(svc.VpsBackend, action)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/v1/vps/%s/%d/%s", svc.VpsBackend, serviceID, seg)
	return c.Post(ctx, path, nil, nil)
}

// VpsStart boots a stopped VPS.
func (c *Client) VpsStart(ctx context.Context, id int) error {
	return c.vpsPower(ctx, id, "start")
}

// VpsStop force-stops a VPS (ACPI-less; may corrupt unwritten data).
// Prefer VpsShutdown for normal use.
func (c *Client) VpsStop(ctx context.Context, id int) error {
	return c.vpsPower(ctx, id, "stop")
}

// VpsShutdown sends an ACPI shutdown signal. Falls back to power-off
// after a timeout if the guest doesn't respond.
func (c *Client) VpsShutdown(ctx context.Context, id int) error {
	return c.vpsPower(ctx, id, "shutdown")
}

// VpsReboot restarts the guest.
func (c *Client) VpsReboot(ctx context.Context, id int) error {
	return c.vpsPower(ctx, id, "reboot")
}

// VpsSetHostname changes the VPS hostname. The server expects PUT
// (not POST) — mutation of an existing attribute, not creation.
// Some Cloud images apply the change only on next boot.
func (c *Client) VpsSetHostname(ctx context.Context, serviceID int, hostname string) error {
	svc, err := c.AccountServiceShow(ctx, serviceID)
	if err != nil {
		return err
	}
	if svc.VpsBackend == "" {
		return fmt.Errorf("service %d is not a VPS", serviceID)
	}
	path := fmt.Sprintf("/v1/vps/%s/%d/hostname", svc.VpsBackend, serviceID)
	return c.Put(ctx, path, map[string]string{"hostname": hostname}, nil)
}

// VpsSetPassword changes the root password. PUT on the password
// attribute (same semantics as VpsSetHostname). Plaintext on the
// wire (HTTPS-secured); server hashes and forwards to the backend.
func (c *Client) VpsSetPassword(ctx context.Context, serviceID int, password string) error {
	svc, err := c.AccountServiceShow(ctx, serviceID)
	if err != nil {
		return err
	}
	if svc.VpsBackend == "" {
		return fmt.Errorf("service %d is not a VPS", serviceID)
	}
	path := fmt.Sprintf("/v1/vps/%s/%d/password", svc.VpsBackend, serviceID)
	return c.Put(ctx, path, map[string]string{"password": password}, nil)
}

// VpsReinstallRequest is the body for VpsReinstall.
type VpsReinstallRequest struct {
	Template string `json:"template"`         // OS template identifier (e.g. "debian-12")
	Password string `json:"password"`         // new root password (required)
	Confirm  bool   `json:"confirm"`          // must be true; safety gate
}

// VpsReinstall reinstalls the OS template on a VPS. **Destructive** —
// wipes everything. Confirm must be true.
//
// On Proxmox: returns an Operation that the caller can Wait() on.
// On Cloud: synchronous (returns nil Operation, nil error on success).
func (c *Client) VpsReinstall(ctx context.Context, serviceID int, req VpsReinstallRequest) (*Operation, error) {
	if !req.Confirm {
		return nil, errors.New("VpsReinstall: req.Confirm must be true (data-loss safety gate)")
	}
	svc, err := c.AccountServiceShow(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.VpsBackend == "" {
		return nil, fmt.Errorf("service %d is not a VPS", serviceID)
	}
	path := fmt.Sprintf("/v1/vps/%s/%d/reinstall", svc.VpsBackend, serviceID)

	if svc.VpsBackend == "proxmox" {
		var op Operation
		if err := c.Post(ctx, path, req, &op); err != nil {
			return nil, err
		}
		return c.bindOperation(&op, serviceID), nil
	}
	// Cloud is synchronous — no Operation future.
	if err := c.Post(ctx, path, req, nil); err != nil {
		return nil, err
	}
	return nil, nil
}

// VpsMigrateRequest is the body for VpsMigrate.
type VpsMigrateRequest struct {
	Target string `json:"target"` // server_id or group_id at the target node
}

// VpsMigrate moves a Proxmox VPS to another node. Returns an
// Operation future — migration is always async upstream.
//
// Cloud doesn't support customer-driven migration; returns an error
// if called on a cloud service.
func (c *Client) VpsMigrate(ctx context.Context, serviceID int, req VpsMigrateRequest) (*Operation, error) {
	svc, err := c.AccountServiceShow(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.VpsBackend != "proxmox" {
		return nil, fmt.Errorf("VpsMigrate is Proxmox-only (service %d has backend=%s)", serviceID, svc.VpsBackend)
	}
	path := fmt.Sprintf("/v1/vps/proxmox/%d/migrate", serviceID)
	var op Operation
	if err := c.Post(ctx, path, req, &op); err != nil {
		return nil, err
	}
	return c.bindOperation(&op, serviceID), nil
}

// VpsCancel submits an AddCancelRequest for a VPS service. Same
// underlying endpoint as ServiceCancel (POST /v1/services/{id}/cancel) —
// staff approves the actual termination. Customer never terminates
// services directly.
func (c *Client) VpsCancel(ctx context.Context, serviceID int, cancelType, reason string) error {
	return c.ServiceCancel(ctx, serviceID, cancelType, reason)
}
