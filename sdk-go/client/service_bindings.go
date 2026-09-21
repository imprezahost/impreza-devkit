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

// MariaDB/MySQL engine (catalog provider `mariadb`). Distinct protocol names:
// an agent that only knows the PostgreSQL family must refuse these manifests.
// MariaDB has no role ownership or login disabling: a dedicated database plus
// a per-revision `user@'%'` with schema-scoped grants, and retirement drops
// the login. No legacy v1 exists for this engine.
const MysqlServiceBindingGenerationProtocol = "mysql-service-binding-v2"
const MysqlServiceBindingGenerationRetirementProtocol = "mysql-service-binding-retire-v2"
const MysqlServiceBindingRotationProtocol = "mysql-service-binding-rotation-v1"

// ServiceBindingBackupProtocol authorizes one reviewed backup transport job to
// read the current generation's credential just in time. The agent derives a
// separate verification login that never reaches the persisted manifest.
const ServiceBindingBackupProtocol = "postgres-service-binding-backup-v2"

// MysqlServiceBindingBackupProtocol is the MariaDB engine's backup
// authorization: same one-job scope, but the scratch verification and the
// dump stage speak mariadb-dump over the binding network.
const MysqlServiceBindingBackupProtocol = "mysql-service-binding-backup-v1"

// BackupDatabaseSpec is the reviewed database stage of a backup transport job.
// References contain no credentials; the agent authorizes them for the current
// command only. VerifyDatabase is the scratch name the agent creates on the
// provider for the mandatory verified restore.
type BackupDatabaseSpec struct {
	ServiceBindingRef
	Protocol             string `json:"protocol"`
	ConsumerDeploymentID string `json:"consumer_deployment_id"`
	VerifyDatabase       string `json:"verify_database"`
}

// ServiceBindingRestoreProtocol authorizes one reviewed restore transport job
// to read the current generation's credential just in time and to land the
// verified dump in a NEW database. It never touches the serving one.
const ServiceBindingRestoreProtocol = "postgres-service-binding-restore-v2"

// MysqlServiceBindingRestoreProtocol is the MariaDB engine's restore
// authorization: same one-job scope, landing the verified dump in a NEW
// database created through the provider's administrator channel.
const MysqlServiceBindingRestoreProtocol = "mysql-service-binding-restore-v1"

// RestoreDatabaseSpec is the reviewed database stage of a restore transport
// job. References contain no credentials; the agent authorizes them for the
// current command only. RestoreDatabase is the new database the agent creates
// on the provider; the live database is never named as a target.
type RestoreDatabaseSpec struct {
	ServiceBindingRef
	Protocol             string `json:"protocol"`
	ConsumerDeploymentID string `json:"consumer_deployment_id"`
	RestoreDatabase      string `json:"restore_database"`
	ExpectedTables       int    `json:"expected_tables"`
	DumpSHA256           string `json:"dump_sha256"`
}

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

// DatabaseRestoreResult is the durable outcome of a reviewed database
// restore: the NEW database, verified by an agent-side table count against
// the reviewed expectation. The live database is never named here — it was
// never touched.
type DatabaseRestoreResult struct {
	RestoreID string `json:"restore_id"`
	Database  string `json:"database"`
	Tables    int    `json:"tables"`
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

// AgentServiceBindingBackup authorizes the database stage of the exact backup
// transport job being executed. The path carries the job's own deployment id.
func (c *Client) AgentServiceBindingBackup(ctx context.Context, jobDeploymentID, commandID, controlToken string) (*ServiceBindingCredentials, error) {
	if !regexp.MustCompile(`^bkpjob_[a-f0-9]{16}$`).MatchString(jobDeploymentID) {
		return nil, errors.New("invalid backup job identity")
	}
	var result ServiceBindingCredentials
	err := c.Post(ctx, "/v1/agent/service-binding-backup/"+url.PathEscape(jobDeploymentID), map[string]string{"command_id": commandID, "control_token": controlToken}, &result)
	return &result, err
}

// AgentServiceBindingRestore authorizes the database stage of the exact
// restore transport job being executed. The path carries the job's own
// deployment id.
func (c *Client) AgentServiceBindingRestore(ctx context.Context, jobDeploymentID, commandID, controlToken string) (*ServiceBindingCredentials, error) {
	if !regexp.MustCompile(`^rstjob_[a-f0-9]{16}$`).MatchString(jobDeploymentID) {
		return nil, errors.New("invalid restore job identity")
	}
	var result ServiceBindingCredentials
	err := c.Post(ctx, "/v1/agent/service-binding-restores/"+url.PathEscape(jobDeploymentID), map[string]string{"command_id": commandID, "control_token": controlToken}, &result)
	return &result, err
}
