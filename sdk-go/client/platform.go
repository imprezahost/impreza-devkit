package client

// Platform-realm client surface — the `/v1/platform/*` endpoints used
// by CLI, panel, and third-party integrations to manage the app
// catalog, deployments, and managed servers. See `../../specs/openapi-platform.yaml`
// in `impreza/impreza-platform` for the canonical contract.
//
// Authentication is the standard Impreza API key + secret (same as
// every other resource — Client built via New, not NewAgent).

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// Shared types — referenced by both agent.go and platform.go
// ─────────────────────────────────────────────────────────────────────

// AppManifest is the fully resolved, server-side-validated manifest sent
// to the agent inside a DeployPayload. Server fills in pinned image
// versions, evaluated vars, etc. — the agent does not need to fetch any
// external repo to act on it.
type AppManifest struct {
	Name          string                  `json:"name"`
	Version       string                  `json:"version"`
	Runtime       ManifestRuntime         `json:"runtime"`
	Lifecycle     ManifestLifecycle       `json:"lifecycle"`
	Network       *ManifestNetwork        `json:"network,omitempty"`
	Observability *ManifestObservability  `json:"observability,omitempty"`
}

// ManifestRuntime describes how the app actually runs on the host.
//
// Build (Phase 12 Iteration 3) opts the deployment into the
// "build the image locally from a tarball" path. When set, the
// agent's docker executor:
//  1. Downloads the tarball from BuildContext.URL (agent-auth GET).
//  2. Verifies BuildContext.SHA256 against the downloaded bytes.
//  3. Extracts into <appDir>/build-ctx/ (relative path the synthesized
//     compose YAML's `build.context: ./build-ctx` directive references).
//  4. Continues with the normal compose-up path; `docker compose up`
//     itself runs `docker build` via the build directive.
//
// Build is mutually exclusive with the bare `image:` directive that
// catalog manifests use. Mixing both in the same compose YAML is the
// customer's footgun (Docker compose will pick `build:` and ignore
// the image hint).
type ManifestRuntime struct {
	Type        string         `json:"type"`                 // docker-compose | docker | systemd | raw
	Isolated    bool           `json:"isolated,omitempty"`
	ComposeYAML string         `json:"compose_yaml,omitempty"`
	DataDir     *DataDirConfig `json:"data_dir,omitempty"`
	Build       *BuildContext  `json:"build,omitempty"`
}

// BuildContext tells the agent to fetch + extract a build context
// tarball before `docker compose up`. Used by Phase 12 Iteration 3
// Dockerfile-mode custom deploys (the customer uploads a gzip
// tarball of their project; the server hands the agent a one-time
// signed reference to that tarball; the agent downloads + extracts +
// hands off to compose's `build:` directive which runs the actual
// docker build).
//
// URL is a full https:// URL or a control-plane-relative path
// (server-side controls which — see PlatformController). The agent
// requests it via GetRaw with its standard agent-auth headers; the
// server validates the calling agent matches the context's
// consumed_by_agent_id before streaming the blob.
//
// SHA256 is the lowercase hex digest of the tarball as the server
// recorded it at upload time. The agent recomputes after download
// and fails the deploy on mismatch (defense against silent corruption
// or a misrouted blob).
//
// DockerfilePath is relative to the extracted tarball root. Default
// (empty) means "Dockerfile" at the root. Customers with a non-default
// layout (apps/myapp/Dockerfile) pass that path here.
type BuildContext struct {
	URL            string `json:"url,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
	SizeBytes      int64  `json:"size_bytes,omitempty"`
	DockerfilePath string `json:"dockerfile_path,omitempty"`

	// Phase 15 — git-clone source. When Git is set, the agent skips
	// the URL/SHA256 tarball-download path and `git clone --depth=1
	// --branch=<ref>` instead, materializing the build context from
	// the repository's HEAD on that ref. URL + SHA256 are then
	// optional (still used for tarball mode; ignored for git).
	//
	// v1 is public-only: BuildContextGit has no DeployKey field yet.
	// Private repos can be unlocked later with either a DeployKey or
	// a short-lived token; the JSON contract is forward-compatible.
	Git *BuildContextGit `json:"git,omitempty"`
}

// BuildContextGit pins a Dockerfile-mode custom deploy to a git
// repository's HEAD on a given ref. The agent runs:
//
//	git clone --depth=1 --branch=<Ref> --single-branch <URL> <destDir>
//
// then proceeds with the standard build path (compose `build:`
// directive runs `docker build` against the cloned tree).
//
// CommitSHA is informational for v1 — the agent doesn't currently
// verify HEAD matches, but the server records it on the deployment
// row so `impreza platform deployments show` can surface what's
// actually deployed. Iteration B will add a checksum gate so a
// force-push between webhook-fire and agent-poll doesn't sneak a
// different commit in.
type BuildContextGit struct {
	URL       string `json:"url"`              // https://github.com/foo/bar.git
	Ref       string `json:"ref,omitempty"`    // branch, tag, or commit; default "main"
	CommitSHA string `json:"commit_sha,omitempty"`
}

// DataDirConfig declares how the agent should prepare the bind-mount
// data directory before `docker compose up`. Letting the manifest
// pick the mode + owner replaces a class of per-app workarounds:
// apps whose image entrypoint properly chowns its own subdirs only
// need `/data` to be traversable (mode 0755) so the dropped-to user
// can enter it. Apps that need their image's native uid to own the
// whole tree can pass an explicit Owner.
//
// When DataDir is nil the agent applies the safe default (mode
// 0700, owned by root). Mode is parsed as octal (e.g. "0755", "750").
// Owner is "uid:gid" or "uid" — both numeric.
type DataDirConfig struct {
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
}

// ManifestLifecycle holds the script paths (or inline shell) for each
// lifecycle hook. Empty fields are treated as no-op.
type ManifestLifecycle struct {
	Install   string `json:"install,omitempty"`
	Update    string `json:"update,omitempty"`
	Rollback  string `json:"rollback,omitempty"`
	Uninstall string `json:"uninstall,omitempty"`
	Health    string `json:"health,omitempty"`
	Backup    string `json:"backup,omitempty"`
}

// ManifestNetwork carries the routing intent — exposed ports + how the
// reverse-proxy should bind them.
type ManifestNetwork struct {
	Exposed      []ManifestPort  `json:"exposed,omitempty"`
	ReverseProxy *ManifestRP     `json:"reverse_proxy,omitempty"`
}

// ManifestPort identifies one exposed service.
type ManifestPort struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // http | https | tcp | udp
}

// ManifestRP describes the reverse-proxy binding.
type ManifestRP struct {
	Enabled bool             `json:"enabled"`
	Routes  []ManifestRPRoute `json:"routes,omitempty"`
}

// ManifestRPRoute matches a hostname/path to a backend service.
type ManifestRPRoute struct {
	Service string `json:"service"`
	Match   string `json:"match"`
}

// ManifestObservability declares where the app's logs and metrics live.
type ManifestObservability struct {
	Logs            []ManifestLogSource `json:"logs,omitempty"`
	MetricsEndpoint *string             `json:"metrics_endpoint,omitempty"`
}

// ManifestLogSource is one source of logs the agent should tail/ship.
type ManifestLogSource struct {
	Container string `json:"container,omitempty"`
	Path      string `json:"path,omitempty"`
}

// Route is a clearnet or onion binding for a deployment.
type Route struct {
	Hostname   string      `json:"hostname"`
	TargetPort int         `json:"target_port,omitempty"`
	// Upstream is the literal `container:port` string the agent's
	// reverse proxy should forward this hostname to (Phase 9.4+).
	// Server resolves any `{deployment_id}` placeholders before sending.
	// When empty the agent falls back to TargetPort + container name
	// derived from the manifest.
	Upstream string      `json:"upstream,omitempty"`
	TLS      *RouteTLS   `json:"tls,omitempty"`
	Onion    *RouteOnion `json:"onion,omitempty"`
}

// RouteTLS configures the certificate provisioning for the hostname.
//
// DNSProvider (Phase 9.11d) opts the route into an ACME DNS-01 challenge
// instead of the default HTTP-01. Set to the lowercase provider name
// (currently only "cloudflare" is supported) when the agent's Caddy
// build can resolve the corresponding `dns ...` module. Required for
// hostnames behind a CDN/proxy (e.g. Cloudflare-proxied imprezaapps
// subdomains) because the proxy intercepts the HTTP-01 challenge path.
//
// When DNSProvider is empty the agent uses HTTP-01 (the historical
// behavior) — appropriate for any hostname that resolves directly to
// the agent's public IP.
type RouteTLS struct {
	Mode        string `json:"mode"`                   // letsencrypt | manual | none
	Email       string `json:"email,omitempty"`        // required for letsencrypt
	DNSProvider string `json:"dns_provider,omitempty"` // "cloudflare" | "" (default = HTTP-01)
}

// RouteOnion turns on the `.onion` mirror of the route.
type RouteOnion struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address,omitempty"` // populated by the agent after Tor sets it up
}

// ─────────────────────────────────────────────────────────────────────
// Catalog — /v1/platform/apps
// ─────────────────────────────────────────────────────────────────────

// App is a single entry in the public catalog.
type App struct {
	Name        string             `json:"name"`
	DisplayName string             `json:"display_name"`
	Version     string             `json:"version"`
	Category    string             `json:"category"`
	Tags        []string           `json:"tags,omitempty"`
	Description string             `json:"description,omitempty"`
	IconURL     string             `json:"icon_url,omitempty"`
	ReadmeURL   string             `json:"readme_url,omitempty"`
	ManifestURL string             `json:"manifest_url,omitempty"`
	Requires    *AppRequirements   `json:"requirements,omitempty"`
	Supports    *AppSupports       `json:"supports,omitempty"`
}

// AppRequirements declares minimum-host requirements an app needs.
type AppRequirements struct {
	RAMMB    int   `json:"ram_mb,omitempty"`
	DiskGB   int   `json:"disk_gb,omitempty"`
	CPUCores int   `json:"cpu_cores,omitempty"`
	Ports    []int `json:"ports,omitempty"`
}

// AppSupports captures optional capabilities an app advertises.
type AppSupports struct {
	Onion        bool `json:"onion,omitempty"`
	CustomDomain bool `json:"custom_domain,omitempty"`
	LetsEncrypt  bool `json:"letsencrypt,omitempty"`
}

// AppListParams narrows the catalog listing.
type AppListParams struct {
	Category string
	Search   string
}

// AppList wraps the catalog response.
type AppList struct {
	Apps  []App `json:"apps"`
	Total int   `json:"total"`
}

// PlatformListApps returns the public catalog. Filters on category /
// search are optional; pass a zero-value AppListParams to get everything.
func (c *Client) PlatformListApps(ctx context.Context, p AppListParams) (*AppList, error) {
	q := url.Values{}
	if p.Category != "" {
		q.Set("category", p.Category)
	}
	if p.Search != "" {
		q.Set("search", p.Search)
	}
	var out AppList
	if err := c.Get(ctx, "/v1/platform/apps", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformGetApp returns the full manifest for a single app.
func (c *Client) PlatformGetApp(ctx context.Context, name string) (*App, error) {
	if name == "" {
		return nil, fmt.Errorf("app name is required")
	}
	var out App
	if err := c.Get(ctx, "/v1/platform/apps/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ─────────────────────────────────────────────────────────────────────
// Deployments — /v1/platform/deployments
// ─────────────────────────────────────────────────────────────────────

// DeploymentStatus is the lifecycle phase of a deployment.
type DeploymentStatus string

const (
	DeploymentPending      DeploymentStatus = "pending"
	DeploymentInstalling   DeploymentStatus = "installing"
	DeploymentRunning      DeploymentStatus = "running"
	DeploymentUpdating     DeploymentStatus = "updating"
	DeploymentRollingBack  DeploymentStatus = "rolling_back"
	DeploymentFailed       DeploymentStatus = "failed"
	DeploymentUninstalling DeploymentStatus = "uninstalling"
	DeploymentUninstalled  DeploymentStatus = "uninstalled"
)

// Deployment is one app instance running on one agent.
type Deployment struct {
	ID           string           `json:"id"`
	AppName      string           `json:"app_name"`
	AppVersion   string           `json:"app_version"`
	AgentID      string           `json:"agent_id"`
	Status       DeploymentStatus `json:"status"`
	Domain       string           `json:"domain,omitempty"`
	Onion        string           `json:"onion,omitempty"`
	Vars         map[string]any   `json:"vars,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	LastHealthAt *time.Time       `json:"last_health_at,omitempty"`
	LastError    string           `json:"last_error,omitempty"`
}

// DeploymentListParams narrows the deployments listing.
type DeploymentListParams struct {
	AgentID string
	Status  string
}

// DeploymentList wraps the listing response.
type DeploymentList struct {
	Deployments []Deployment `json:"deployments"`
	Total       int          `json:"total"`
}

// DeploymentCreateRequest is the body of POST /v1/platform/deployments.
type DeploymentCreateRequest struct {
	AppName    string         `json:"app_name"`
	AppVersion string         `json:"app_version,omitempty"` // empty → latest
	AgentID    string         `json:"agent_id"`
	Vars       map[string]any `json:"vars,omitempty"`
	Domain     string         `json:"domain,omitempty"`
	Onion      bool           `json:"onion,omitempty"`
}

// PlatformListDeployments returns deployments owned by the authenticated
// client, optionally filtered.
func (c *Client) PlatformListDeployments(ctx context.Context, p DeploymentListParams) (*DeploymentList, error) {
	q := url.Values{}
	if p.AgentID != "" {
		q.Set("agent_id", p.AgentID)
	}
	if p.Status != "" {
		q.Set("status", p.Status)
	}
	var out DeploymentList
	if err := c.Get(ctx, "/v1/platform/deployments", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformCreateDeployment kicks off a new deployment. Returns
// immediately with the created Deployment; clients poll
// PlatformGetDeployment to follow progress.
func (c *Client) PlatformCreateDeployment(ctx context.Context, req DeploymentCreateRequest) (*Deployment, error) {
	var out Deployment
	if err := c.Post(ctx, "/v1/platform/deployments", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformGetDeployment fetches a deployment by id.
func (c *Client) PlatformGetDeployment(ctx context.Context, id string) (*Deployment, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	var out Deployment
	if err := c.Get(ctx, "/v1/platform/deployments/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformRollbackRequest is the body of POST .../rollback.
type PlatformRollbackRequest struct {
	TargetVersion string `json:"target_version"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
	Confirm       bool   `json:"confirm"` // must be true
}

// PlatformRollback enqueues a rollback to a prior version. Confirm
// MUST be true (server enforces the gate; included here to match the
// REST contract).
func (c *Client) PlatformRollback(ctx context.Context, id string, req PlatformRollbackRequest) error {
	if id == "" {
		return fmt.Errorf("deployment id is required")
	}
	if !req.Confirm {
		return fmt.Errorf("rollback requires confirm=true")
	}
	return c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/rollback", req, nil)
}

// PlatformUninstallRequest is the body of POST .../uninstall.
type PlatformUninstallRequest struct {
	PurgeData bool `json:"purge_data,omitempty"`
	Confirm   bool `json:"confirm"` // must be true
}

// PlatformLifecycleResponse is what the server returns for in-flight
// lifecycle commands (uninstall, restart, future update/rollback).
// Wraps the new command id + current deployment state so callers can
// poll progress without a second GET.
type PlatformLifecycleResponse struct {
	CommandID  string     `json:"command_id"`
	Deployment Deployment `json:"deployment"`
}

// PlatformUninstall enqueues uninstall of an app instance. Confirm
// MUST be true. Returns the new command id + the deployment row with
// status flipped to `uninstalling`.
func (c *Client) PlatformUninstall(ctx context.Context, id string, req PlatformUninstallRequest) (*PlatformLifecycleResponse, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	if !req.Confirm {
		return nil, fmt.Errorf("uninstall requires confirm=true")
	}
	var out PlatformLifecycleResponse
	if err := c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/uninstall", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformRestartRequest is the body of POST .../restart. Empty for
// now; reserved for future per-command flags.
type PlatformRestartRequest struct{}

// PlatformRestart enqueues a docker-compose restart of the
// deployment's services. Non-destructive; no confirm gate.
func (c *Client) PlatformRestart(ctx context.Context, id string, req PlatformRestartRequest) (*PlatformLifecycleResponse, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	var out PlatformLifecycleResponse
	if err := c.Post(ctx, "/v1/platform/deployments/"+url.PathEscape(id)+"/restart", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ─────────────────────────────────────────────────────────────────────
// Custom deployments — /v1/platform/deployments/custom (Phase 12)
// ─────────────────────────────────────────────────────────────────────
//
// A "custom deployment" is an app that doesn't live in the curated
// catalog — typically generated by an AI tool (Claude Code, Cursor,
// Codex, etc.) or shipped by a customer who wants to host their own
// container without writing a manifest. Three modes are supported,
// auto-detected server-side from the request body shape:
//
//   1. Image       — caller supplies `image: "ghcr.io/user/app:tag"`.
//                     Agent does `docker pull` + `docker run`.
//   2. Dockerfile  — caller supplies inline Dockerfile text OR a Git
//                     URL OR a `context_id` returned by an earlier
//                     PlatformUploadCustomDeployContext call. Agent
//                     builds locally before running.
//   3. Manifest    — caller supplies a full manifest (compose YAML +
//                     lifecycle + vars schema). Treated like a catalog
//                     deployment with the manifest snapshot the caller
//                     provided.
//
// Cgroup limits (cpus, memory_mb) are top-level fields — clamped
// server-side against the VPS's known capacity for Impreza-managed
// agents, trusted verbatim for external/BYO. Defaults are 1.0 CPU
// and 512 MB when omitted; reasonable for v1 anti-abuse posture
// (trust the customer + cgroup ceilings).
//
// Per-customer naming: `name` must be unique within the caller's
// account but does NOT need to be globally unique. Customer X's
// "my-bot" never collides with customer Y's "my-bot".

// CustomDeployMode selects which source flavor a custom deploy uses.
type CustomDeployMode string

const (
	CustomDeployModeImage      CustomDeployMode = "image"
	CustomDeployModeDockerfile CustomDeployMode = "dockerfile"
	CustomDeployModeManifest   CustomDeployMode = "manifest"
)

// CustomDeployRequest is the body of POST /v1/platform/deployments/custom.
//
// Mode auto-detection (server-side): the server inspects which of
// `image`, `dockerfile` / `git_url` / `context_id`, or `manifest`
// is populated and routes accordingly. Providing fields from more
// than one mode is a 400 INVALID_REQUEST.
type CustomDeployRequest struct {
	// Common fields ---------------------------------------------------
	Name       string         `json:"name"`                  // per-client unique slug
	AgentID    string         `json:"agent_id"`              // target server
	Domain     string         `json:"domain,omitempty"`      // empty → auto-subdomain
	Onion      bool           `json:"onion,omitempty"`       // also publish via Tor
	Vars       map[string]any `json:"vars,omitempty"`        // env vars for container(s)
	Cpus       float64        `json:"cpus,omitempty"`        // default 1.0
	MemoryMB   int            `json:"memory_mb,omitempty"`   // default 512
	TargetPort int            `json:"target_port,omitempty"` // default 80 (port the container listens on)

	// Mode-specific source --------------------------------------------
	// Image mode:
	Image string `json:"image,omitempty"`

	// Dockerfile mode (pick exactly one):
	Dockerfile     string `json:"dockerfile,omitempty"`      // inline Dockerfile text
	GitURL         string `json:"git_url,omitempty"`         // https (public / PAT) or SSH (deploy_key) Git URL
	GitRef         string `json:"git_ref,omitempty"`         // tag/branch/commit, default "main"
	ContextID      string `json:"context_id,omitempty"`      // from PlatformUploadCustomDeployContext
	DockerfilePath string `json:"dockerfile_path,omitempty"` // override path within the context (default "Dockerfile")

	// Private-git auth (git_url mode). "" / "none" = public; "deploy_key"
	// = SSH (the server generates a keypair and returns the public half
	// in the response's GitAuth.PublicKey to add as a read-only Deploy
	// Key); "pat" = HTTPS with a fine-grained token supplied in GitPat.
	GitAuthMethod string `json:"git_auth_method,omitempty"`
	GitPat        string `json:"git_pat,omitempty"`

	// Manifest mode:
	Manifest *AppManifest `json:"manifest,omitempty"`
}

// CustomDeployment is one row in imprezaplatform_custom_deployments.
// Shares most lifecycle fields with Deployment (uninstall/restart/logs
// endpoints work identically — they look up by deployment_id across
// both tables) but is exposed as a distinct type so callers can tell
// at a glance whether an entry is a catalog install or a custom build.
type CustomDeployment struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Mode         CustomDeployMode `json:"mode"`
	AgentID      string           `json:"agent_id"`
	Status       DeploymentStatus `json:"status"`
	Domain       string           `json:"domain,omitempty"`
	Onion        string           `json:"onion,omitempty"`
	Image        string           `json:"image,omitempty"`
	GitURL       string           `json:"git_url,omitempty"`
	GitRef       string           `json:"git_ref,omitempty"`
	GitAuth      *GitAuthInfo     `json:"git_auth,omitempty"`
	Cpus         float64          `json:"cpus"`
	MemoryMB     int              `json:"memory_mb"`
	Vars         map[string]any   `json:"vars,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	LastHealthAt *time.Time       `json:"last_health_at,omitempty"`
	LastError    string           `json:"last_error,omitempty"`
}

// GitAuthInfo surfaces the non-secret git-auth state of a custom
// deployment: the method, a credential fingerprint, and (for deploy_key)
// the public key to add to the repo as a read-only Deploy Key. The
// secret itself is never returned.
type GitAuthInfo struct {
	Method      string `json:"method"`               // none | deploy_key | pat
	Fingerprint string `json:"fingerprint,omitempty"`
	PublicKey   string `json:"public_key,omitempty"` // deploy_key only
}

// CustomDeploymentList wraps the listing response.
type CustomDeploymentList struct {
	Deployments []CustomDeployment `json:"deployments"`
	Total       int                `json:"total"`
}

// PlatformCreateCustomDeployment kicks off a new custom deployment.
// Returns immediately with the persisted CustomDeployment; clients
// poll PlatformGetCustomDeployment to follow status.
func (c *Client) PlatformCreateCustomDeployment(ctx context.Context, req CustomDeployRequest) (*CustomDeployment, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("custom deploy: name is required")
	}
	if req.AgentID == "" {
		return nil, fmt.Errorf("custom deploy: agent_id is required")
	}
	var out CustomDeployment
	if err := c.Post(ctx, "/v1/platform/deployments/custom", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformListCustomDeployments returns the caller's custom deployments,
// optionally narrowed to a single agent.
func (c *Client) PlatformListCustomDeployments(ctx context.Context, agentID string) (*CustomDeploymentList, error) {
	q := url.Values{}
	if agentID != "" {
		q.Set("agent_id", agentID)
	}
	var out CustomDeploymentList
	if err := c.Get(ctx, "/v1/platform/deployments/custom", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlatformGetCustomDeployment fetches a single custom deployment.
func (c *Client) PlatformGetCustomDeployment(ctx context.Context, id string) (*CustomDeployment, error) {
	if id == "" {
		return nil, fmt.Errorf("deployment id is required")
	}
	var out CustomDeployment
	if err := c.Get(ctx, "/v1/platform/deployments/custom/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CustomDeployContextUpload is the response of POST
// /v1/platform/deployments/custom/contexts — a one-time, expiring
// reference to a build-context tarball the server staged on disk.
// Plug ContextID into a subsequent CustomDeployRequest{ContextID: ...}
// with mode=dockerfile to consume it.
type CustomDeployContextUpload struct {
	ContextID string    `json:"context_id"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PlatformUploadCustomDeployContext uploads a gzip-compressed tarball
// (the customer's project directory) to the control plane and returns
// a one-time CustomDeployContextUpload reference. The tarball MUST be
// gzip-compressed; the server enforces a size cap (default 100 MB,
// configurable via mod_imprezaapi_config.custom_deploy_context_max_mb).
//
// The returned ContextID has a 24-hour TTL by default — if you don't
// reference it in a CustomDeployRequest within that window the
// server's daily sweep reclaims the disk space.
//
// Phase 12 Iteration 3 — Dockerfile mode + local-tarball shipping.
func (c *Client) PlatformUploadCustomDeployContext(ctx context.Context, tarball []byte) (*CustomDeployContextUpload, error) {
	var out CustomDeployContextUpload
	if err := c.PostRaw(ctx, "/v1/platform/deployments/custom/contexts", "application/gzip", tarball, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ─────────────────────────────────────────────────────────────────────
// Servers — /v1/platform/servers
// ─────────────────────────────────────────────────────────────────────

// ServerOrigin classifies how a managed server entered the platform.
type ServerOrigin string

const (
	OriginImprezaProxmox    ServerOrigin = "impreza-proxmox"
	OriginImprezaCloud      ServerOrigin = "impreza-cloud"
	OriginImprezaDedicated  ServerOrigin = "impreza-dedicated"
	OriginExternal          ServerOrigin = "external"
)

// AgentStatus is the connectivity state of a managed server.
type AgentStatus string

const (
	AgentPending  AgentStatus = "pending"
	AgentOnline   AgentStatus = "online"
	AgentOffline  AgentStatus = "offline"
	AgentDraining AgentStatus = "draining"
	AgentRevoked  AgentStatus = "revoked"
)

// Server is one managed server entry. Service-side it's an alias of
// the `agents` row joined with the source service (when Impreza-owned).
type Server struct {
	AgentID    string       `json:"agent_id"`
	Hostname   string       `json:"hostname"`
	Origin     ServerOrigin `json:"origin"`
	ServiceID  *int         `json:"service_id,omitempty"` // nil for external
	Status     AgentStatus  `json:"status"`
	Version    string       `json:"version,omitempty"`
	LastSeenAt *time.Time   `json:"last_seen_at,omitempty"`
}

// ServerList wraps the servers response.
type ServerList struct {
	Servers []Server `json:"servers"`
	Total   int      `json:"total"`
}

// PlatformListServers returns every managed server visible to the
// authenticated client (Impreza-provisioned + external bring-your-own).
func (c *Client) PlatformListServers(ctx context.Context) (*ServerList, error) {
	var out ServerList
	if err := c.Get(ctx, "/v1/platform/servers", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExternalBootstrapRequest is the body of POST .../servers/external/bootstrap.
type ExternalBootstrapRequest struct {
	Label string `json:"label,omitempty"`
}

// ExternalBootstrapResponse returns the one-time install command the
// client runs on the foreign server.
type ExternalBootstrapResponse struct {
	BootstrapToken string    `json:"bootstrap_token"`
	InstallURL     string    `json:"install_url"`
	OneLiner       string    `json:"one_liner"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// PlatformIssueExternalBootstrap creates a one-time bootstrap token for
// a bring-your-own server and returns the curl|sh one-liner. The token
// is single-use and expires in ~10 minutes.
func (c *Client) PlatformIssueExternalBootstrap(ctx context.Context, req ExternalBootstrapRequest) (*ExternalBootstrapResponse, error) {
	var out ExternalBootstrapResponse
	if err := c.Post(ctx, "/v1/platform/servers/external/bootstrap", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ─────────────────────────────────────────────────────────────────────
// Helper: build a query string for ad-hoc int filters (for use by
// future endpoints — keeps url.Values usage uniform).
// ─────────────────────────────────────────────────────────────────────

func intParam(v int) string { return strconv.Itoa(v) }
