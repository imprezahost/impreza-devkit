package client

import (
	"context"
	"fmt"
)

// VpsList wraps GET /v1/account/services and filters down to entries
// with a non-empty vps_backend field. The server doesn't expose a
// standalone `/vps` listing; the Python SDK does the same client-side
// filter.
//
// `backend` further filters to "proxmox" or "cloud" only.
// `status` is forwarded as the server-side query parameter.
func (c *Client) VpsList(ctx context.Context, backend, status string) ([]Service, error) {
	all, err := c.AccountServices(ctx, status)
	if err != nil {
		return nil, err
	}
	out := make([]Service, 0, len(all))
	for _, s := range all {
		if s.VpsBackend == "" {
			continue
		}
		if backend != "" && s.VpsBackend != backend {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// VpsShow wraps GET /v1/account/services/{id}. Returns the same
// service shape as VpsList rows plus the per-service extras
// (username, server hostname, etc.). The vps_backend field tells
// callers which backend the service runs on, useful for follow-up
// calls to the backend-specific routes.
func (c *Client) VpsShow(ctx context.Context, id int) (*Service, error) {
	s, err := c.AccountServiceShow(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.VpsBackend == "" {
		return nil, fmt.Errorf("service %d is not a VPS (backend not set)", id)
	}
	return s, nil
}

// VpsStatusInfo is the live power-state response from
// GET /v1/vps/{backend}/{id}/status. Proxmox returns the rich shape
// with CPU + memory; Cloud returns just power_state.
type VpsStatusInfo struct {
	PowerState   string  `json:"power_state"`
	CPUUsage     float64 `json:"cpu_usage,omitempty"`
	MemoryUsed   int64   `json:"memory_used,omitempty"`   // bytes
	MemoryTotal  int64   `json:"memory_total,omitempty"`  // bytes
	Uptime       int     `json:"uptime,omitempty"`        // seconds
}

// VpsStatus wraps the per-backend status endpoints:
//
//   - Proxmox: GET /v1/vps/proxmox/{id}/status — typed shape
//     (power_state, cpu_usage, memory_used/total, uptime).
//   - Cloud:   GET /v1/vps/cloud/{id} — richer info object;
//     we extract power_state + memory_total from the top level.
//     CPU + uptime + memory_used aren't reported on Cloud.
//
// Resolves the backend by first fetching the service detail (one
// extra HTTP call, mirroring the Python SDK).
func (c *Client) VpsStatus(ctx context.Context, id int) (string, *VpsStatusInfo, error) {
	svc, err := c.AccountServiceShow(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if svc.VpsBackend == "" {
		return "", nil, fmt.Errorf("service %d is not a VPS (backend not set)", id)
	}

	switch svc.VpsBackend {
	case "proxmox":
		var s VpsStatusInfo
		path := fmt.Sprintf("/v1/vps/proxmox/%d/status", id)
		if err := c.Get(ctx, path, nil, &s); err != nil {
			return svc.VpsBackend, nil, err
		}
		return svc.VpsBackend, &s, nil

	case "cloud":
		// Cloud-side status comes from the full info endpoint —
		// no dedicated /status route. Decode just the fields the
		// Cloud backend reports at the top level.
		var cloud struct {
			PowerState  string `json:"power_state"`
			MemoryTotal int64  `json:"memory_total"`
		}
		path := fmt.Sprintf("/v1/vps/cloud/%d", id)
		if err := c.Get(ctx, path, nil, &cloud); err != nil {
			return svc.VpsBackend, nil, err
		}
		return svc.VpsBackend, &VpsStatusInfo{
			PowerState:  cloud.PowerState,
			MemoryTotal: cloud.MemoryTotal,
		}, nil

	default:
		return svc.VpsBackend, nil, fmt.Errorf("unknown VPS backend: %s", svc.VpsBackend)
	}
}
