// Proxmox VPS sub-resources: snapshots, backups, backup-schedules,
// network reconfigure. Mirrors the Python SDK's vps_proxmox.py
// resource layer.
//
// All endpoints live under /v1/vps/proxmox/{service_id}/...
package client

import (
	"context"
	"fmt"
	"net/url"
)

// ── Snapshots ─────────────────────────────────────────────────────

// Snapshot is one row of GET /v1/vps/proxmox/{id}/snapshots.
type Snapshot struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	Parent      string `json:"parent,omitempty"`
}

type snapshotsListResponse struct {
	Snapshots []Snapshot `json:"snapshots"`
	Total     int        `json:"total"`
}

// ProxmoxSnapshotsList wraps GET /v1/vps/proxmox/{id}/snapshots.
func (c *Client) ProxmoxSnapshotsList(ctx context.Context, serviceID int) ([]Snapshot, error) {
	var resp snapshotsListResponse
	path := fmt.Sprintf("/v1/vps/proxmox/%d/snapshots", serviceID)
	if err := c.Get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Snapshots, nil
}

// ProxmoxSnapshotCreate wraps POST /v1/vps/proxmox/{id}/snapshots.
func (c *Client) ProxmoxSnapshotCreate(ctx context.Context, serviceID int, name, description string) (*Snapshot, error) {
	body := map[string]string{"name": name}
	if description != "" {
		body["description"] = description
	}
	var snap Snapshot
	path := fmt.Sprintf("/v1/vps/proxmox/%d/snapshots", serviceID)
	if err := c.Post(ctx, path, body, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// ProxmoxSnapshotDelete wraps DELETE /v1/vps/proxmox/{id}/snapshots/{name}.
func (c *Client) ProxmoxSnapshotDelete(ctx context.Context, serviceID int, name string) error {
	path := fmt.Sprintf("/v1/vps/proxmox/%d/snapshots/%s", serviceID, url.PathEscape(name))
	return c.Delete(ctx, path, nil)
}

// ProxmoxSnapshotRollback wraps POST /v1/vps/proxmox/{id}/snapshots/{name}/rollback.
// Returns an Operation that the caller can Wait() on.
func (c *Client) ProxmoxSnapshotRollback(ctx context.Context, serviceID int, name string) (*Operation, error) {
	var op Operation
	path := fmt.Sprintf("/v1/vps/proxmox/%d/snapshots/%s/rollback", serviceID, url.PathEscape(name))
	if err := c.Post(ctx, path, nil, &op); err != nil {
		return nil, err
	}
	return c.bindOperation(&op, serviceID), nil
}

// ── Backups ───────────────────────────────────────────────────────

// Backup is one row of GET /v1/vps/proxmox/{id}/backups.
type Backup struct {
	ID         any    `json:"id"` // server may emit string or int — both work via any
	Filename   string `json:"filename,omitempty"`
	Size       int64  `json:"size,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	Storage    string `json:"storage,omitempty"`
	Volid      string `json:"volid,omitempty"`
}

type backupsListResponse struct {
	Backups []Backup `json:"backups"`
	Total   int      `json:"total"`
}

// ProxmoxBackupsList wraps GET /v1/vps/proxmox/{id}/backups.
func (c *Client) ProxmoxBackupsList(ctx context.Context, serviceID int) ([]Backup, error) {
	var resp backupsListResponse
	path := fmt.Sprintf("/v1/vps/proxmox/%d/backups", serviceID)
	if err := c.Get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Backups, nil
}

// ProxmoxBackupCreate wraps POST /v1/vps/proxmox/{id}/backups.
// Long-running — returns an Operation for Wait().
func (c *Client) ProxmoxBackupCreate(ctx context.Context, serviceID int) (*Operation, error) {
	var op Operation
	path := fmt.Sprintf("/v1/vps/proxmox/%d/backups", serviceID)
	if err := c.Post(ctx, path, nil, &op); err != nil {
		return nil, err
	}
	return c.bindOperation(&op, serviceID), nil
}

// ProxmoxBackupRestore wraps POST /v1/vps/proxmox/{id}/backups/{backup_id}/restore.
// Long-running — returns an Operation.
func (c *Client) ProxmoxBackupRestore(ctx context.Context, serviceID int, backupID string) (*Operation, error) {
	var op Operation
	path := fmt.Sprintf("/v1/vps/proxmox/%d/backups/%s/restore", serviceID, url.PathEscape(backupID))
	if err := c.Post(ctx, path, nil, &op); err != nil {
		return nil, err
	}
	return c.bindOperation(&op, serviceID), nil
}

// ProxmoxBackupDelete wraps DELETE /v1/vps/proxmox/{id}/backups/{backup_id}.
func (c *Client) ProxmoxBackupDelete(ctx context.Context, serviceID int, backupID string) error {
	path := fmt.Sprintf("/v1/vps/proxmox/%d/backups/%s", serviceID, url.PathEscape(backupID))
	return c.Delete(ctx, path, nil)
}

// ── BackupSchedules ───────────────────────────────────────────────

// BackupSchedule is one row of GET /v1/vps/proxmox/{id}/backup-schedules.
//
// Server emits a single `starttime` string (e.g. "03:15:00"), not
// separate hour/minute integers — that's the field the Proxmox
// upstream stores. Create accepts hour+minute and the server
// composes them into starttime.
type BackupSchedule struct {
	ID        any    `json:"id"`
	Dow       string `json:"dow,omitempty"`       // e.g. "mon,wed,fri"
	Starttime string `json:"starttime,omitempty"` // "HH:MM:SS"
	Mode      string `json:"mode,omitempty"`      // snapshot | suspend | stop
	Compress  string `json:"compress,omitempty"`  // zstd | lzo | gzip | none
}

type schedulesListResponse struct {
	Schedules []BackupSchedule `json:"schedules"`
	Count     int              `json:"count"` // server emits "count", not "total"
}

// ProxmoxBackupSchedulesList wraps GET /v1/vps/proxmox/{id}/backup-schedules.
func (c *Client) ProxmoxBackupSchedulesList(ctx context.Context, serviceID int) ([]BackupSchedule, error) {
	var resp schedulesListResponse
	path := fmt.Sprintf("/v1/vps/proxmox/%d/backup-schedules", serviceID)
	if err := c.Get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Schedules, nil
}

// BackupScheduleCreateRequest is the body for POST .../backup-schedules.
// Field names match what the Proxmox upstream consumes.
type BackupScheduleCreateRequest struct {
	Dow      string `json:"dow"`                 // e.g. "mon,wed,fri"
	Hour     int    `json:"hour"`                // 0-23
	Minute   int    `json:"minute"`              // 0-59
	Mode     string `json:"mode,omitempty"`      // snapshot | suspend | stop
	Compress string `json:"compress,omitempty"`  // zstd | lzo | gzip | none
}

// ProxmoxBackupScheduleCreate wraps POST .../backup-schedules.
func (c *Client) ProxmoxBackupScheduleCreate(ctx context.Context, serviceID int, req BackupScheduleCreateRequest) (*BackupSchedule, error) {
	var s BackupSchedule
	path := fmt.Sprintf("/v1/vps/proxmox/%d/backup-schedules", serviceID)
	if err := c.Post(ctx, path, req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ProxmoxBackupScheduleDelete wraps DELETE .../backup-schedules/{id}.
// scheduleID accepted as string for max flexibility (server may return
// numeric or string ids depending on Proxmox version).
func (c *Client) ProxmoxBackupScheduleDelete(ctx context.Context, serviceID int, scheduleID string) error {
	path := fmt.Sprintf("/v1/vps/proxmox/%d/backup-schedules/%s", serviceID, url.PathEscape(scheduleID))
	return c.Delete(ctx, path, nil)
}

// ── Network reconfigure (Proxmox-only inline verb) ────────────────

// ProxmoxNetworkReconfigure wraps POST /v1/vps/proxmox/{id}/network/reconfigure.
// Triggers a guest-agent or reboot-driven reconfigure of the VPS network
// stack. Returns the raw `data` payload (server emits a status object
// that varies upstream — keep it loose).
func (c *Client) ProxmoxNetworkReconfigure(ctx context.Context, serviceID int) (map[string]any, error) {
	var data map[string]any
	path := fmt.Sprintf("/v1/vps/proxmox/%d/network/reconfigure", serviceID)
	if err := c.Post(ctx, path, nil, &data); err != nil {
		return nil, err
	}
	return data, nil
}
