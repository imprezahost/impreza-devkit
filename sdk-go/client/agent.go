package client

// Agent-realm client surface — the `/v1/agent/*` endpoints consumed by
// the impreza-agent daemon. See the published OpenAPI spec for the
// canonical contract.
//
// Two distinct flows live here:
//
//  1. Bootstrap: a package-level function that exchanges a one-time
//     bearer token for permanent agent credentials. No Client needed.
//
//  2. Long-lived operations (poll, report, deploy-result, logs):
//     methods on a Client built via NewAgent.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/imprezahost/impreza-devkit/sdk-go/tor"
)

// ─────────────────────────────────────────────────────────────────────
// Command kinds
// ─────────────────────────────────────────────────────────────────────

// CommandKind identifies one of the kinds of command an agent
// may receive from the control plane via long-poll.
type CommandKind string

const (
	CommandDeploy       CommandKind = "deploy"
	CommandUpdate       CommandKind = "update"
	CommandRollback     CommandKind = "rollback"
	CommandUninstall    CommandKind = "uninstall"
	CommandRestart      CommandKind = "restart"
	CommandHealthCheck  CommandKind = "health_check"
	CommandLogsTail     CommandKind = "logs_tail"
	CommandAgentUpgrade CommandKind = "agent_upgrade"
	// CommandUpdateRoutes (Phase 9.19) re-routes a running deployment
	// to a new hostname without restarting its containers. The agent
	// regenerates its Caddyfile fragment + reloads Caddy in-place;
	// the container + Tor hidden service are left running as-is.
	CommandUpdateRoutes CommandKind = "update_routes"
)

// ─────────────────────────────────────────────────────────────────────
// Bootstrap — exchange a one-time token for permanent credentials
// ─────────────────────────────────────────────────────────────────────

// BootstrapRequest is the body posted to /v1/agent/bootstrap.
type BootstrapRequest struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`            // e.g. "ubuntu-22.04"
	Arch         string `json:"arch"`          // "amd64" or "arm64"
	AgentVersion string `json:"agent_version"` // semver of the daemon
	PublicIP     string `json:"public_ip,omitempty"`
}

// BootstrapResponse is the JSON payload returned on successful bootstrap.
// The secret is shown ONCE and cannot be retrieved later — the agent
// MUST persist it to /etc/impreza-agent/credentials.toml at 0600 before
// exiting bootstrap.
type BootstrapResponse struct {
	AgentID             string `json:"agent_id"`
	AgentSecret         string `json:"agent_secret"`
	ControlPlaneURL     string `json:"control_plane_url"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
}

// BootstrapOptions configures Bootstrap. UseTor / Proxy are honored
// because the agent may bootstrap from a Tor-only environment.
type BootstrapOptions struct {
	BaseURL string
	UseTor  bool
	Proxy   string
	Timeout time.Duration
}

// Bootstrap exchanges a one-time bootstrap token for permanent agent
// credentials. Call this from the agent's `bootstrap` command, then
// persist the response. The bootstrap token is invalidated server-side
// on success; callers cannot retry with the same token.
//
// Unlike the rest of the SDK, Bootstrap does not require a long-lived
// Client because the credentials it returns are what makes one possible.
func Bootstrap(ctx context.Context, token string, req BootstrapRequest, opts BootstrapOptions) (*BootstrapResponse, error) {
	proxyURL := opts.Proxy
	if proxyURL == "" && opts.UseTor {
		proxyURL = tor.DefaultSOCKS
	}
	base, err := tor.Transport(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("build bootstrap transport: %w", err)
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	httpClient := &http.Client{Transport: base, Timeout: timeout}

	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode bootstrap request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/agent/bootstrap", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build bootstrap request: %w", err)
	}
	// Send the bootstrap token in BOTH a Bearer header and a custom
	// X-Bootstrap-Token header. Apache / PHP-FPM commonly strips
	// Authorization unless a SetEnvIf / RewriteRule forwards it; the
	// custom header survives unconditionally. The server picks up
	// whichever it sees.
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("X-Bootstrap-Token", token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent())

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST /v1/agent/bootstrap: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap response: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode bootstrap envelope: %w (raw=%q)", err, string(raw))
	}
	if !env.Success || env.Error != nil {
		code, msg := "", ""
		if env.Error != nil {
			code, msg = env.Error.Code, env.Error.Message
		}
		return nil, mapStatus(resp.StatusCode, code, msg, requestIDFromHeader(resp))
	}

	var out BootstrapResponse
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("decode bootstrap data: %w", err)
	}
	return &out, nil
}

// ─────────────────────────────────────────────────────────────────────
// Long-poll: receive a command
// ─────────────────────────────────────────────────────────────────────

// PollRequest is the optional body of POST /v1/agent/poll. Used by
// tests to shorten the long-poll wait; production agents pass nil.
type PollRequest struct {
	WaitSeconds int `json:"wait_seconds,omitempty"`
}

// PollCommand is the command envelope returned by /v1/agent/poll. The
// concrete payload type depends on Kind — use the typed accessors
// (DeployPayload, UpdatePayload, etc.) to decode it.
type PollCommand struct {
	ID       string          `json:"id"`
	Kind     CommandKind     `json:"kind"`
	Payload  json.RawMessage `json:"payload"`
	Deadline *time.Time      `json:"deadline,omitempty"`
}

// AgentPoll blocks up to ~55s waiting for a command. The (nil, false,
// nil) return — no command, no error — indicates a clean empty poll;
// the agent should reconnect immediately. This corresponds to a 204
// from the server (handled transparently inside Post → do).
func (c *Client) AgentPoll(ctx context.Context, req *PollRequest) (*PollCommand, bool, error) {
	var cmd PollCommand
	if err := c.Post(ctx, "/v1/agent/poll", req, &cmd); err != nil {
		return nil, false, err
	}
	// Empty body — either from a 204 (which decodes into a zero-value
	// PollCommand) or from a "success but no command" envelope. Treat
	// both the same so callers don't have to.
	if cmd.ID == "" {
		return nil, false, nil
	}
	return &cmd, true, nil
}

// ─────────────────────────────────────────────────────────────────────
// Typed command payloads (decoded from PollCommand.Payload)
// ─────────────────────────────────────────────────────────────────────

// DeployPayload is the payload of a CommandDeploy.
type DeployPayload struct {
	DeploymentID string         `json:"deployment_id"`
	Manifest     AppManifest    `json:"manifest"`
	Vars         map[string]any `json:"vars,omitempty"`
	Routes       []Route        `json:"routes,omitempty"`
	// GitAuthMethod tells the agent how to authenticate a private git
	// clone: "" / "none" (public), "deploy_key" (SSH), or "pat" (https
	// token). The credential itself is NOT in the payload — the agent
	// fetches it just-in-time via AgentGitCredential at clone time.
	GitAuthMethod string `json:"git_auth_method,omitempty"`
}

// UpdatePayload is the payload of a CommandUpdate.
type UpdatePayload struct {
	DeploymentID string      `json:"deployment_id"`
	Manifest     AppManifest `json:"manifest"`
	FromVersion  string      `json:"from_version,omitempty"`
	ToVersion    string      `json:"to_version,omitempty"`
}

// RollbackPayload is the payload of a CommandRollback.
type RollbackPayload struct {
	DeploymentID  string  `json:"deployment_id"`
	TargetVersion string  `json:"target_version"`
	SnapshotID    *string `json:"snapshot_id,omitempty"`
}

// UninstallPayload is the payload of a CommandUninstall.
type UninstallPayload struct {
	DeploymentID string `json:"deployment_id"`
	PurgeData    bool   `json:"purge_data,omitempty"`
}

// RestartPayload is the payload of a CommandRestart.
type RestartPayload struct {
	DeploymentID string `json:"deployment_id"`
}

// HealthCheckPayload is the payload of a CommandHealthCheck.
type HealthCheckPayload struct {
	DeploymentID string `json:"deployment_id"`
}

// LogsTailPayload is the payload of a CommandLogsTail.
type LogsTailPayload struct {
	DeploymentID string `json:"deployment_id"`
	StreamID     string `json:"stream_id"`
	Follow       bool   `json:"follow,omitempty"`
	SinceSeconds int    `json:"since_seconds,omitempty"`
}

// AgentUpgradePayload is the payload of a CommandAgentUpgrade.
type AgentUpgradePayload struct {
	TargetVersion string `json:"target_version"`
	PackageURL    string `json:"package_url"`
	Checksum      string `json:"checksum"` // sha256 hex
}

// UpdateRoutesPayload is the payload of a CommandUpdateRoutes
// (Phase 9.19). The server sends this to swap a deployment's clearnet
// hostname (or otherwise mutate its route set) without taking the
// container or Tor hidden service offline.
//
// Behavior on the agent side:
//   - When Vars is non-empty, the agent atomically rewrites the
//     deployment's .env file. The container is NOT restarted — the
//     new DOMAIN_URL etc. only take effect on the next manual
//     `restart` command (or on a future `update` redeploy).
//   - The agent then writes a new Caddyfile fragment for the
//     deployment using Routes + OnionAddr, regenerates the aggregate
//     Caddyfile, and calls `caddy reload`. Reload is zero-downtime
//     and idempotent on identical content.
//   - When OnionAddr is non-empty AND a Route has Onion.Enabled, the
//     agent emits the matching `http://<addr> {}` Caddy block again
//     so the onion endpoint survives the reload. Tor itself is NOT
//     restarted — the hidden service keeps publishing the same
//     address it had before the command.
//   - Phase 89: when ProvisionOnion is true AND OnionAddr is empty,
//     the agent provisions a brand-new hidden service for the
//     deployment BEFORE applying the routes. The newly-published
//     address is reported back in DeployResult.Onion so the server
//     can persist it on imprezaplatform_deployments.onion. Already-
//     populated OnionAddr short-circuits the provisioning (idempotent
//     — server should only set ProvisionOnion when row.onion is
//     empty, but defense in depth).
type UpdateRoutesPayload struct {
	DeploymentID   string         `json:"deployment_id"`
	Routes         []Route        `json:"routes,omitempty"`
	Vars           map[string]any `json:"vars,omitempty"`
	OnionAddr      string         `json:"onion_addr,omitempty"`
	ProvisionOnion bool           `json:"provision_onion,omitempty"`
}

// As decodes c.Payload into the typed target. Returns an error if the
// JSON does not match the target struct. The caller is expected to
// pick the right target based on c.Kind.
func (c *PollCommand) As(target any) error {
	if len(c.Payload) == 0 {
		return fmt.Errorf("empty payload for command %s", c.ID)
	}
	return json.Unmarshal(c.Payload, target)
}

// ─────────────────────────────────────────────────────────────────────
// Reporting back to the control plane
// ─────────────────────────────────────────────────────────────────────

// AgentLoad is the resource-usage snapshot included in heartbeats.
type AgentLoad struct {
	CPUPercent  float64 `json:"cpu_pct"`
	MemUsedMB   uint64  `json:"mem_used_mb"`
	MemTotalMB  uint64  `json:"mem_total_mb"`
	DiskUsedGB  uint64  `json:"disk_used_gb"`
	DiskTotalGB uint64  `json:"disk_total_gb"`
}

// RunningDeployment is the per-deployment status the agent reports on
// every heartbeat. Used by the panel to show running/degraded apps.
type RunningDeployment struct {
	DeploymentID    string `json:"deployment_id"`
	Status          string `json:"status"`
	ContainerHealth string `json:"container_health"`
}

// AgentReport is the heartbeat body POSTed every ~30s.
type AgentReport struct {
	ReportedAt         time.Time           `json:"reported_at"`
	Version            string              `json:"version,omitempty"`
	Load               *AgentLoad          `json:"load,omitempty"`
	RunningDeployments []RunningDeployment `json:"running_deployments,omitempty"`
}

// AgentReport sends a heartbeat + status report to the control plane.
// Three consecutive missed reports cause the agent to be marked offline.
func (c *Client) AgentReport(ctx context.Context, r AgentReport) error {
	return c.Post(ctx, "/v1/agent/report", r, nil)
}

// DeployResult is the result body POSTed by the agent when it finishes
// any command (deploy, update, rollback, uninstall, restart, etc.).
// Idempotent on CommandID — re-posting the same result is a no-op.
type DeployResult struct {
	CommandID        string            `json:"command_id"`
	Status           string            `json:"status"` // success | failed | timeout | partial
	DeploymentID     string            `json:"deployment_id,omitempty"`
	Domain           string            `json:"domain,omitempty"`
	Onion            string            `json:"onion,omitempty"`
	AdminCredentials map[string]string `json:"admin_credentials,omitempty"`
	Error            string            `json:"error,omitempty"`
	LogsTail         string            `json:"logs_tail,omitempty"`
}

// AgentDeployResult reports the outcome of a command back to the
// control plane.
func (c *Client) AgentDeployResult(ctx context.Context, result DeployResult) error {
	return c.Post(ctx, "/v1/agent/deploy-result", result, nil)
}

// GitCredential is the just-in-time private-git credential returned by
// GET /v1/agent/git-credential/{deployment_id}: the decrypted deploy-key
// private half (Method "deploy_key") or the PAT (Method "pat"). The agent
// uses it transiently at clone time and never persists it.
type GitCredential struct {
	Method     string `json:"method"`
	Credential string `json:"credential"`
}

// AgentGitCredential fetches the private-git credential for a deployment
// the calling agent owns. Only call it when the deploy payload's
// GitAuthMethod is "deploy_key" or "pat"; the control plane returns 404
// for a public deployment or one owned by a different agent.
func (c *Client) AgentGitCredential(ctx context.Context, deploymentID string) (*GitCredential, error) {
	var cred GitCredential
	if err := c.Get(ctx, "/v1/agent/git-credential/"+deploymentID, nil, &cred); err != nil {
		return nil, err
	}
	return &cred, nil
}

// LogChunk is one chunk of streamed logs (up to 256 KB) posted in
// response to a logs_tail command.
type LogChunk struct {
	StreamID string `json:"stream_id"`
	Chunk    string `json:"chunk"`
	Final    bool   `json:"final,omitempty"`
}

// AgentLogs sends one chunk of logs to the control plane. Set Final to
// true on the last chunk to close the stream.
func (c *Client) AgentLogs(ctx context.Context, chunk LogChunk) error {
	return c.Post(ctx, "/v1/agent/logs", chunk, nil)
}
