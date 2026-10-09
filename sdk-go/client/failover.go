package client

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

var failoverDeploymentID = regexp.MustCompile(`^dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})$`)
var failoverCutoverID = regexp.MustCompile(`^fov_[a-f0-9]{24}$`)
var failoverBackupID = regexp.MustCompile(`^bkp_[a-f0-9]{16}$`)
var failoverCommandID = regexp.MustCompile(`^cmd_[a-f0-9]{16,32}$`)
var failoverDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var failbackID = regexp.MustCompile(`^fbk_[a-f0-9]{24}$`)

// FailoverSync names the exact backup, restore and healthy redeploy receipts.
// Confirming does not copy data; the server revalidates ownership and freshness.
type FailoverSync struct {
	BackupID        string `json:"backup_id"`
	RestoreID       string `json:"restore_id"`
	DeployCommandID string `json:"deploy_command_id"`
}
type FailoverStandby struct {
	SourceDeploymentID string          `json:"source_deployment_id"`
	TargetDeploymentID string          `json:"target_deployment_id"`
	Hostname           string          `json:"hostname,omitempty"`
	Mode               string          `json:"mode,omitempty"`
	State              string          `json:"state"`
	SourceCountry      string          `json:"source_country,omitempty"`
	TargetCountry      string          `json:"target_country,omitempty"`
	LastSyncAt         *string         `json:"last_sync_at,omitempty"`
	LastVerifiedAt     *string         `json:"last_verified_at,omitempty"`
	SyncReceipt        json.RawMessage `json:"sync_receipt,omitempty"`
	Receipt            json.RawMessage `json:"receipt,omitempty"`
	// Drill holds the drill cadence, the active drill and the last one.
	Drill json.RawMessage `json:"drill,omitempty"`
}

// FailoverDrill is one restore drill of a cold standby. Verified means its
// receipts passed the cold sync check; durations describe that drill only.
type FailoverDrill struct {
	DrillID            string          `json:"drill_id"`
	Status             string          `json:"status"`
	TriggeredBy        string          `json:"triggered_by"`
	SourceDeploymentID string          `json:"source_deployment_id"`
	TargetDeploymentID string          `json:"target_deployment_id"`
	BackupID           *string         `json:"backup_id"`
	RestoreID          *string         `json:"restore_id"`
	StartedAt          *string         `json:"started_at"`
	FinishedAt         *string         `json:"finished_at"`
	Failure            *string         `json:"failure"`
	Receipt            json.RawMessage `json:"receipt"`
}

var failoverDrillID = regexp.MustCompile(`^fdr_[a-f0-9]{24}$`)

// PlatformSetFailoverDrillPolicy schedules standby drills every interval
// (60 to 10080 minutes); nil turns them off. Each drill overwrites the
// standby's data with a fresh verified copy of the primary.
func (c *Client) PlatformSetFailoverDrillPolicy(ctx context.Context, id string, intervalMinutes *int) (*FailoverStandby, error) {
	path, err := failoverDeploymentPath(id)
	if err != nil {
		return nil, err
	}
	if intervalMinutes != nil && (*intervalMinutes < 60 || *intervalMinutes > 10080) {
		return nil, fmt.Errorf("drill interval must be from 60 to 10080 minutes")
	}
	var out FailoverStandby
	if err = c.Post(ctx, path+"/failover-standby/drill-policy", map[string]*int{"interval_minutes": intervalMinutes}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformRunFailoverDrill starts a drill now; read it until verified or failed.
func (c *Client) PlatformRunFailoverDrill(ctx context.Context, id string) (*FailoverDrill, error) {
	path, err := failoverDeploymentPath(id)
	if err != nil {
		return nil, err
	}
	var out FailoverDrill
	if err = c.Post(ctx, path+"/failover-standby/drills", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) PlatformGetFailoverDrill(ctx context.Context, id string) (*FailoverDrill, error) {
	if !failoverDrillID.MatchString(id) {
		return nil, fmt.Errorf("invalid failover drill id")
	}
	var out FailoverDrill
	if err := c.Get(ctx, "/v1/platform/failover-drills/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FailoverReview expires after 15 minutes. Preparing never switches traffic.
// Inspect Summary and preserve the exact digest for the user's confirmation.
type FailoverReview struct {
	CutoverID    string          `json:"cutover_id"`
	ReviewDigest string          `json:"review_digest"`
	Status       string          `json:"status"`
	ExpiresAt    string          `json:"expires_at"`
	Summary      json.RawMessage `json:"summary"`
	Receipt      json.RawMessage `json:"receipt"`
}
type FailoverConfirmation struct {
	ReviewDigest string `json:"review_digest"`
	Confirm      bool   `json:"confirm"`
}

// FailoverAccepted is an asynchronous receipt, not proof of a completed cutover.
// Read PlatformGetFailover until verified or an actionable failure is reported.
type FailoverAccepted struct {
	Receipt  json.RawMessage `json:"receipt"`
	Replayed bool            `json:"replayed"`
}

func failoverDeploymentPath(id string) (string, error) {
	if !failoverDeploymentID.MatchString(id) {
		return "", fmt.Errorf("invalid failover deployment id")
	}
	return "/v1/platform/deployments/custom/" + id, nil
}
func failoverPath(id string) (string, error) {
	if !failoverCutoverID.MatchString(id) {
		return "", fmt.Errorf("invalid failover cutover id")
	}
	return "/v1/platform/failover-cutovers/" + id, nil
}

// PlatformPairColdStandby pairs existing apps; it does not create a VPS or copy data.
func (c *Client) PlatformPairColdStandby(ctx context.Context, sourceID, targetID string) (*FailoverStandby, error) {
	path, err := failoverDeploymentPath(sourceID)
	if err != nil {
		return nil, err
	}
	if !failoverDeploymentID.MatchString(targetID) || targetID == sourceID {
		return nil, fmt.Errorf("choose a different valid standby deployment")
	}
	var out FailoverStandby
	err = c.Post(ctx, path+"/failover-standby", map[string]string{"mode": "cold", "target_deployment_id": targetID}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) PlatformGetFailoverStandby(ctx context.Context, id string) (*FailoverStandby, error) {
	path, err := failoverDeploymentPath(id)
	if err != nil {
		return nil, err
	}
	var out FailoverStandby
	if err = c.Get(ctx, path+"/failover-standby", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) PlatformConfirmFailoverSync(ctx context.Context, id string, receipts FailoverSync) (*FailoverStandby, error) {
	path, err := failoverDeploymentPath(id)
	if err != nil {
		return nil, err
	}
	if !failoverBackupID.MatchString(receipts.BackupID) || !failoverBackupID.MatchString(receipts.RestoreID) || !failoverCommandID.MatchString(receipts.DeployCommandID) {
		return nil, fmt.Errorf("invalid failover sync receipt identifiers")
	}
	var out FailoverStandby
	if err = c.Post(ctx, path+"/failover-standby/confirm-sync", receipts, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) PlatformPrepareFailover(ctx context.Context, id string) (*FailoverReview, error) {
	path, err := failoverDeploymentPath(id)
	if err != nil {
		return nil, err
	}
	var out FailoverReview
	if err = c.Post(ctx, path+"/prepare-failover", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) PlatformGetFailover(ctx context.Context, id string) (*FailoverReview, error) {
	path, err := failoverPath(id)
	if err != nil {
		return nil, err
	}
	var out FailoverReview
	if err = c.Get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformApplyFailover stops and fences the source and may lose writes newer
// than the copied backup. Obtain explicit user confirmation of this review.
func (c *Client) PlatformApplyFailover(ctx context.Context, id string, confirmation FailoverConfirmation) (*FailoverAccepted, error) {
	return c.failoverConfirmed(ctx, id, "/apply", confirmation)
}

// PlatformRetryFailoverActivation does not clear the source fence. The server
// bounds retries and revalidates the original ownership, route and DNS.
func (c *Client) PlatformRetryFailoverActivation(ctx context.Context, id string, confirmation FailoverConfirmation) (*FailoverAccepted, error) {
	return c.failoverConfirmed(ctx, id, "/retry-activation", confirmation)
}
func (c *Client) failoverConfirmed(ctx context.Context, id, suffix string, confirmation FailoverConfirmation) (*FailoverAccepted, error) {
	path, err := failoverPath(id)
	if err != nil {
		return nil, err
	}
	return c.confirmedPost(ctx, path+suffix, confirmation)
}
func (c *Client) confirmedPost(ctx context.Context, path string, confirmation FailoverConfirmation) (*FailoverAccepted, error) {
	if !confirmation.Confirm || !failoverDigest.MatchString(confirmation.ReviewDigest) {
		return nil, fmt.Errorf("failover requires confirm=true and the exact reviewed digest")
	}
	var out FailoverAccepted
	if err := c.Post(ctx, path, confirmation, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FailbackReview expires after 15 minutes. It lifts the old primary's fence
// and inverts the pair; it starts no containers and moves no traffic.
type FailbackReview struct {
	FailbackID   string          `json:"failback_id"`
	ReviewDigest string          `json:"review_digest"`
	Status       string          `json:"status"`
	ExpiresAt    string          `json:"expires_at"`
	Summary      json.RawMessage `json:"summary"`
	Receipt      json.RawMessage `json:"receipt"`
}

func failbackPath(id string) (string, error) {
	if !failbackID.MatchString(id) {
		return "", fmt.Errorf("invalid failback id")
	}
	return "/v1/platform/failover-failbacks/" + id, nil
}

// PlatformPrepareFailback reviews the failback of a verified cutover.
func (c *Client) PlatformPrepareFailback(ctx context.Context, cutoverID string) (*FailbackReview, error) {
	path, err := failoverPath(cutoverID)
	if err != nil {
		return nil, err
	}
	var out FailbackReview
	if err = c.Post(ctx, path+"/prepare-failback", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) PlatformGetFailback(ctx context.Context, id string) (*FailbackReview, error) {
	path, err := failbackPath(id)
	if err != nil {
		return nil, err
	}
	var out FailbackReview
	if err = c.Get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformApplyFailback releases the old primary's fence. Acceptance only
// queues the release; read PlatformGetFailback until inverted, then sync the
// current primary into the old one and review the return cutover.
func (c *Client) PlatformApplyFailback(ctx context.Context, id string, confirmation FailoverConfirmation) (*FailoverAccepted, error) {
	path, err := failbackPath(id)
	if err != nil {
		return nil, err
	}
	return c.confirmedPost(ctx, path+"/apply", confirmation)
}
