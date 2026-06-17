package executor

// Docker is the docker-compose-driven executor: the real implementation
// that replaces Echo for deployments declaring
// `manifest.runtime.type = "docker-compose"`.
//
// Per-deployment state lives under
// `<StateDir>/apps/<deployment_id>/` with:
//
//	compose.yaml   — verbatim from manifest.runtime.compose_yaml
//	.env           — KEY=VALUE pairs from DeployPayload.vars
//	data/          — bind-mounted into containers (per-app convention)
//
// All command kinds defined in the SDK route through Execute(); unknown
// or not-yet-implemented kinds fall back to Echo so a synthetic test
// command (e.g. `health_check` against a nonexistent deployment) still
// gets a clean success reply.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const (
	// Per-command timeouts. `docker compose pull` is the slow one;
	// keep the others tight so an agent doesn't sit on a stuck command.
	composePullTimeout    = 5 * time.Minute
	composeUpTimeout      = 2 * time.Minute
	composeDownTimeout    = 2 * time.Minute
	composeRestartTimeout = 1 * time.Minute
	composeQueryTimeout   = 30 * time.Second
)

// Docker is the runtime executor. Build with NewDocker.
type Docker struct {
	StateDir string // absolute path; agent passes this from internal/state.
	Log      *slog.Logger
	Proxy    *proxy.Caddy // optional — when nil, routes are ignored
	Tor      *proxy.Tor   // optional — when nil, onion is ignored

	// Client is the SDK client used to ship log chunks back to the
	// control plane (for the logs_tail command kind). When nil, the
	// logs_tail handler returns the buffered output via DeployResult.LogsTail
	// instead — useful for tests and when the agent runs in dry-run mode.
	Client *sdkclient.Client
}

// NewDocker returns a Docker executor rooted at stateDir, with a Caddy
// reverse-proxy manager wired to <stateDir>/proxy/, plus a Tor
// manager for onion routes (engaged only when a route requests it).
func NewDocker(stateDir string, log *slog.Logger) *Docker {
	return &Docker{
		StateDir: stateDir,
		Log:      log,
		Proxy:    proxy.New(stateDir, log),
		Tor:      proxy.NewTor(stateDir, log),
	}
}

// Execute dispatches a poll command to the right per-kind handler.
func (d *Docker) Execute(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	switch cmd.Kind {
	case sdkclient.CommandDeploy:
		return d.deploy(ctx, cmd)
	case sdkclient.CommandUninstall:
		return d.uninstall(ctx, cmd)
	case sdkclient.CommandRestart:
		return d.restart(ctx, cmd)
	case sdkclient.CommandHealthCheck:
		return d.healthCheck(ctx, cmd)
	case sdkclient.CommandLogsTail:
		return d.logsTail(ctx, cmd)
	case sdkclient.CommandUpdateRoutes:
		return d.updateRoutes(ctx, cmd)
	default:
		// Kinds we haven't implemented yet (Update, Rollback,
		// AgentUpgrade) fall back to the echo path so the command
		// queue advances and operators get a clear "no-op" in the
		// result rather than a server-side stuck job.
		return Echo{}.Execute(ctx, cmd)
	}
}

// ─────────────────────────────────────────────────────────────────────
// deploy
// ─────────────────────────────────────────────────────────────────────

func (d *Docker) deploy(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.DeployPayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode deploy payload: "+err.Error())
	}
	if p.DeploymentID == "" {
		return failResult(cmd.ID, "deploy payload missing deployment_id")
	}
	if p.Manifest.Runtime.Type != "docker-compose" {
		return failResult(cmd.ID, fmt.Sprintf(
			"unsupported runtime %q (only docker-compose for now)",
			p.Manifest.Runtime.Type,
		))
	}
	composeYAML := strings.TrimSpace(p.Manifest.Runtime.ComposeYAML)
	if composeYAML == "" {
		return failResult(cmd.ID, "manifest has empty compose_yaml")
	}

	appDir := d.appDir(p.DeploymentID)
	dataDir := filepath.Join(appDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return failResult(cmd.ID, "mkdir state dir: "+err.Error())
	}
	// Honor manifest.runtime.data_dir if declared — lets the manifest
	// widen mode (so a non-root container user can traverse the bind
	// mount) and/or pre-chown the tree to the app's native uid:gid
	// instead of forcing every app's compose to use `user: root`.
	// Defaults stay at 0700/root when the field is absent.
	if dd := p.Manifest.Runtime.DataDir; dd != nil {
		if err := applyDataDirConfig(dataDir, dd); err != nil {
			return failResult(cmd.ID, "apply data_dir config: "+err.Error())
		}
	}

	// Phase 12 Iteration 3 — Dockerfile-mode custom deploys ship a
	// BuildContext that the agent must download + extract BEFORE
	// `docker compose up`. Compose's `build:` directive (declared in
	// the server-synthesized compose YAML) then runs `docker build`
	// against the extracted context. Catalog / image / manifest
	// deploys have Build=nil and skip this entire branch.
	if d.Client != nil && p.Manifest.Runtime.Build != nil {
		buildDir := filepath.Join(appDir, "build-ctx")
		if err := fetchAndExtractBuildContext(ctx, d.Client, d.Log, p.Manifest.Runtime.Build, buildDir, p.GitAuthMethod, p.DeploymentID); err != nil {
			return failResult(cmd.ID, "fetch build context: "+err.Error())
		}
		d.Log.Info("docker deploy: build context ready",
			"deployment_id", p.DeploymentID,
			"dir", buildDir,
			"sha256", p.Manifest.Runtime.Build.SHA256,
		)
	} else if p.Manifest.Runtime.Build != nil && d.Client == nil {
		return failResult(cmd.ID,
			"manifest.runtime.build is set but agent has no SDK client wired (cannot download context)",
		)
	}

	d.Log.Info("docker deploy: writing state", "deployment_id", p.DeploymentID, "dir", appDir)

	if err := writeAtomic(filepath.Join(appDir, "compose.yaml"), []byte(composeYAML+"\n"), 0o644); err != nil {
		return failResult(cmd.ID, "write compose.yaml: "+err.Error())
	}
	if err := writeAtomic(filepath.Join(appDir, ".env"), []byte(renderEnv(p.Vars)), 0o600); err != nil {
		return failResult(cmd.ID, "write .env: "+err.Error())
	}

	// 0. If the manifest's compose joins the shared `impreza-proxy`
	//    network OR the deploy carries reverse-proxy routes, ensure
	//    the network + Caddy container are running BEFORE `compose up`.
	//    Compose fails to bring services up if a referenced external
	//    network doesn't exist yet.
	if d.Proxy != nil && (strings.Contains(composeYAML, proxy.NetworkName) || len(p.Routes) > 0) {
		netCtx, cancelNet := context.WithTimeout(ctx, composeQueryTimeout)
		if err := d.Proxy.EnsureNetwork(netCtx); err != nil {
			cancelNet()
			return failResult(cmd.ID, "ensure proxy network: "+err.Error())
		}
		cancelNet()

		// Phase 9.11d v2: the agent's OWN credentials reach Caddy via
		// SetImprezaCredentials at startup (cmd/run.go) — not via
		// per-deploy vars. The CF token never reaches the agent.
		// See caddy-dns-impreza/README.md for the rationale.

		if len(p.Routes) > 0 {
			runCtx, cancelRun := context.WithTimeout(ctx, composeUpTimeout)
			if err := d.Proxy.EnsureRunning(runCtx); err != nil {
				cancelRun()
				return failResult(cmd.ID, "ensure caddy running: "+err.Error())
			}
			cancelRun()
		}
	}

	// Phase 90 — onion-only deploys: provision Tor BEFORE compose pull/up
	// so the container's first boot reads the real .onion address from
	// .env. Detection: any route with empty Hostname + Onion.Enabled
	// is how server-side buildRoutes encodes "onion-only intent" — the
	// route exists but with no clearnet hostname to anchor a Caddy
	// virtualhost. Dual-stack (clearnet + onion) keeps the existing
	// later-provisioning path; the clearnet DOMAIN/DOMAIN_URL is
	// already correct in .env from deploy time, and the onion only
	// supplements it.
	primaryOnion := ""
	onionOnlyIntent := false
	for _, r := range p.Routes {
		if r.Onion != nil && r.Onion.Enabled && r.Hostname == "" {
			onionOnlyIntent = true
			break
		}
	}
	if onionOnlyIntent && d.Tor != nil {
		provCtx, cancelProv := context.WithTimeout(ctx, 3*time.Minute)
		addr, err := d.Tor.ProvisionHiddenService(provCtx, p.DeploymentID, 80)
		cancelProv()
		if err != nil {
			return failResult(cmd.ID, "provision hidden service (onion-only pre-compose): "+err.Error())
		}
		primaryOnion = addr
		// Back-fill DOMAIN + DOMAIN_URL with the published .onion so
		// the container reads the canonical Tor hostname from its
		// first boot, and any app that uses DOMAIN_URL for invite
		// links / OAuth / cookie scope renders correct values from
		// the start.
		if p.Vars == nil {
			p.Vars = map[string]any{}
		}
		p.Vars["DOMAIN"] = addr
		p.Vars["DOMAIN_URL"] = "http://" + addr
		if err := writeAtomic(filepath.Join(appDir, ".env"), []byte(renderEnv(p.Vars)), 0o600); err != nil {
			return failResult(cmd.ID, "rewrite .env with onion vars (onion-only): "+err.Error())
		}
		d.Log.Info("docker deploy: onion-only — Tor provisioned + .env back-filled",
			"deployment_id", p.DeploymentID, "onion", addr)
	}

	// 1. Pull (long).
	d.Log.Info("docker deploy: pulling images", "deployment_id", p.DeploymentID)
	pullCtx, cancelPull := context.WithTimeout(ctx, composePullTimeout)
	defer cancelPull()
	if out, err := d.compose(pullCtx, appDir, "pull"); err != nil {
		return failResult(cmd.ID, fmt.Sprintf("docker compose pull: %v\n%s", err, tail(out, 1536)))
	}

	// 2. Up -d.
	d.Log.Info("docker deploy: bringing stack up", "deployment_id", p.DeploymentID)
	upCtx, cancelUp := context.WithTimeout(ctx, composeUpTimeout)
	defer cancelUp()
	if out, err := d.compose(upCtx, appDir, "up", "-d"); err != nil {
		return failResult(cmd.ID, fmt.Sprintf("docker compose up: %v\n%s", err, tail(out, 1536)))
	}

	// 2.5 Phase 9.9: lifecycle.install hook. Manifest authors put a
	//     wizard-complete shell script here that pokes the app's
	//     setup endpoint with the operator's admin_user / admin_password
	//     vars (when provided). The script must be self-gating —
	//     when no admin password is set, it should exit 0 early so
	//     the same manifest works in both "browser wizard" and
	//     "CLI-pre-configured" modes.
	//
	//     Runs in the appDir with vars + DOMAIN_URL etc. exposed as
	//     env vars. 3-minute budget covers a slow first-boot of e.g.
	//     n8n or Synapse.
	if installScript := strings.TrimSpace(p.Manifest.Lifecycle.Install); installScript != "" {
		d.Log.Info("docker deploy: running lifecycle.install", "deployment_id", p.DeploymentID)
		instCtx, cancelInst := context.WithTimeout(ctx, 3*time.Minute)
		out, err := d.runShellScript(instCtx, appDir, p.Vars, installScript)
		cancelInst()
		if err != nil {
			return failResult(cmd.ID, fmt.Sprintf("lifecycle.install: %v\n%s", err, tail(out, 4096)))
		}
		d.Log.Info("docker deploy: lifecycle.install complete",
			"deployment_id", p.DeploymentID, "bytes_out", len(out))
	}

	// 3. Program Caddy + (optionally) Tor with the deploy's routes.
	//    Idempotent on the deployment_id — a re-deploy overwrites the
	//    prior Caddy fragment, removing any stale hostnames.
	//
	//    Onion path: when ANY route asks for it, ensure Tor is running
	//    AND provision a hidden service for the deployment. The
	//    resulting .onion is added back into each route's `OnionAddr`
	//    so Caddy emits the matching `http://<onion> {}` block at the
	//    same time as the clearnet block — single fragment, atomic
	//    reload, no half-state.
	//
	//    Phase 90: when primaryOnion is ALREADY populated (onion-only
	//    intent handled above, BEFORE compose pull/up), skip the
	//    re-provision — ProvisionHiddenService is idempotent on the
	//    deployment_id but there's no reason to round-trip it twice.
	if d.Proxy != nil && len(p.Routes) > 0 {
		// Decide if Tor is needed.
		needTor := false
		for _, r := range p.Routes {
			if r.Onion != nil && r.Onion.Enabled {
				needTor = true
				break
			}
		}
		if needTor && d.Tor != nil && primaryOnion == "" {
			// ProvisionHiddenService also handles Tor lifecycle (cold
			// launch with the new torrc, or SIGHUP if already
			// running). Tighter 3-minute budget covers a cold start
			// + bootstrap + hidden service publication.
			provCtx, cancelProv := context.WithTimeout(ctx, 3*time.Minute)
			addr, err := d.Tor.ProvisionHiddenService(provCtx, p.DeploymentID, 80)
			cancelProv()
			if err != nil {
				return failResult(cmd.ID, "provision hidden service: "+err.Error())
			}
			primaryOnion = addr
			d.Log.Info("docker deploy: hidden service provisioned",
				"deployment_id", p.DeploymentID, "onion", addr)
		}

		routes := make([]proxy.Route, 0, len(p.Routes))
		for _, r := range p.Routes {
			upstream := r.Upstream
			if upstream == "" {
				// Phase 9.3 fallback path: TargetPort + a best-effort
				// container name guess. Phase 9.4 server should always
				// fill Upstream, so this is just defense in depth.
				upstream = fmt.Sprintf("dpl_%s:%d", p.DeploymentID, r.TargetPort)
			}
			mode := "letsencrypt"
			email := ""
			dnsProvider := ""
			if r.TLS != nil {
				if r.TLS.Mode != "" {
					mode = r.TLS.Mode
				}
				email = r.TLS.Email
				dnsProvider = r.TLS.DNSProvider
			}
			onion := ""
			if r.Onion != nil && r.Onion.Enabled && primaryOnion != "" {
				onion = primaryOnion
			}
			routes = append(routes, proxy.Route{
				Hostname:       r.Hostname,
				OnionAddr:      onion,
				Upstream:       upstream,
				TLSMode:        mode,
				TLSEmail:       email,
				TLSDNSProvider: dnsProvider,
			})
		}
		applyCtx, cancelApply := context.WithTimeout(ctx, composeQueryTimeout)
		if err := d.Proxy.ApplyDeploymentRoutes(applyCtx, p.DeploymentID, routes); err != nil {
			cancelApply()
			d.Log.Warn("docker deploy: caddy route apply failed (deploy itself succeeded)",
				"deployment_id", p.DeploymentID, "err", err)
		}
		cancelApply()

		// Phase 9.23 (2026-05-26) — block on a real HTTPS handshake to
		// the customer's first clearnet route before we report running.
		// Eliminates the install-then-click window where the customer
		// gets ERR_SSL_PROTOCOL_ERROR (cert mid-issuance) or 502
		// (upstream not ready). 90s budget covers the typical
		// fresh-LE-cert + container-warmup path. Onion-only deploys
		// have no clearnet host to probe — ProbeHTTPS returns false
		// immediately for empty host, which is fine (no probe needed).
		var probeHost string
		for _, r := range routes {
			if r.Hostname != "" {
				probeHost = r.Hostname
				break
			}
		}
		if probeHost != "" {
			d.Log.Info("docker deploy: probing https before reporting running",
				"deployment_id", p.DeploymentID, "host", probeHost)
			probeCtx, cancelProbe := context.WithTimeout(ctx, probeBudget+5*time.Second)
			_ = ProbeHTTPS(probeCtx, probeHost, d.Log)
			cancelProbe()
		}
	}

	// 4. Capture a short tail of logs for the result so the panel
	//    shows something concrete on success.
	logsCtx, cancelLogs := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancelLogs()
	logOut, _ := d.compose(logsCtx, appDir, "logs", "--tail=20", "--no-color")

	d.Log.Info("docker deploy: success", "deployment_id", p.DeploymentID, "onion", primaryOnion)
	return sdkclient.DeployResult{
		CommandID:    cmd.ID,
		Status:       "success",
		DeploymentID: p.DeploymentID,
		Domain:       envValue(p.Vars, "DOMAIN_URL"),
		Onion:        primaryOnion,
		LogsTail:     tail(logOut, 4096),
	}
}

// ─────────────────────────────────────────────────────────────────────
// uninstall
// ─────────────────────────────────────────────────────────────────────

func (d *Docker) uninstall(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.UninstallPayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode uninstall payload: "+err.Error())
	}
	appDir := d.appDir(p.DeploymentID)

	if !exists(appDir) {
		// Nothing to do — treat as idempotent success.
		d.Log.Info("docker uninstall: no state dir, no-op", "deployment_id", p.DeploymentID)
		return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID}
	}

	downCtx, cancel := context.WithTimeout(ctx, composeDownTimeout)
	defer cancel()
	args := []string{"down"}
	if p.PurgeData {
		args = append(args, "--volumes")
	}
	if out, err := d.compose(downCtx, appDir, args...); err != nil {
		d.Log.Warn("docker compose down failed, removing state anyway", "deployment_id", p.DeploymentID, "err", err)
		return failResult(cmd.ID, fmt.Sprintf("compose down: %v\n%s", err, tail(out, 1024)))
	}

	if p.PurgeData {
		if err := os.RemoveAll(appDir); err != nil {
			d.Log.Warn("removeAll state dir failed", "deployment_id", p.DeploymentID, "err", err)
		}
	}

	// Remove any Caddy routes that pointed at this deployment so we
	// don't keep serving stale hostnames after an uninstall.
	if d.Proxy != nil {
		rmCtx, cancelRm := context.WithTimeout(ctx, composeQueryTimeout)
		if err := d.Proxy.RemoveDeploymentRoutes(rmCtx, p.DeploymentID); err != nil {
			d.Log.Warn("docker uninstall: caddy route remove failed",
				"deployment_id", p.DeploymentID, "err", err)
		}
		cancelRm()
	}

	// Tear down the hidden service too. We DELETE the keys (so the
	// next reinstall gets a fresh .onion); a future sticky-onion
	// feature would move them into a parking dir instead.
	if d.Tor != nil {
		torRmCtx, cancelTorRm := context.WithTimeout(ctx, composeQueryTimeout)
		if err := d.Tor.RemoveHiddenService(torRmCtx, p.DeploymentID); err != nil {
			d.Log.Warn("docker uninstall: tor service remove failed",
				"deployment_id", p.DeploymentID, "err", err)
		}
		cancelTorRm()
	}
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID}
}

// ─────────────────────────────────────────────────────────────────────
// restart
// ─────────────────────────────────────────────────────────────────────

func (d *Docker) restart(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.RestartPayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode restart payload: "+err.Error())
	}
	appDir := d.appDir(p.DeploymentID)
	if !exists(appDir) {
		return failResult(cmd.ID, "no state dir for deployment "+p.DeploymentID)
	}
	rCtx, cancel := context.WithTimeout(ctx, composeRestartTimeout)
	defer cancel()
	if out, err := d.compose(rCtx, appDir, "restart"); err != nil {
		return failResult(cmd.ID, fmt.Sprintf("compose restart: %v\n%s", err, tail(out, 1024)))
	}
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID}
}

// ─────────────────────────────────────────────────────────────────────
// healthCheck
// ─────────────────────────────────────────────────────────────────────

func (d *Docker) healthCheck(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.HealthCheckPayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode health payload: "+err.Error())
	}
	appDir := d.appDir(p.DeploymentID)
	if !exists(appDir) {
		// Synthetic / smoke commands hit this — return success with a
		// hint, don't poison the queue. (The reaper's regression suite
		// still relies on health_check round-tripping cleanly.)
		return sdkclient.DeployResult{
			CommandID:    cmd.ID,
			Status:       "success",
			DeploymentID: p.DeploymentID,
			LogsTail:     "health_check: no state dir for deployment (no-op).\n",
		}
	}
	psCtx, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancel()
	out, err := d.compose(psCtx, appDir, "ps", "--format", "json")
	if err != nil {
		return failResult(cmd.ID, fmt.Sprintf("compose ps: %v\n%s", err, tail(out, 1024)))
	}
	return sdkclient.DeployResult{
		CommandID:    cmd.ID,
		Status:       "success",
		DeploymentID: p.DeploymentID,
		LogsTail:     tail(out, 2048),
	}
}

// ─────────────────────────────────────────────────────────────────────
// updateRoutes (Phase 9.19)
// ─────────────────────────────────────────────────────────────────────

// updateRoutes swaps the deployment's clearnet hostname (and/or other
// route mutations) without restarting the container or Tor sidecar.
// Three concrete actions:
//
//  1. If Vars is non-empty, atomically rewrite .env. Container is
//     left alone — apps that bake DOMAIN_URL in at startup pick up
//     the new value on the next `restart`. Trade-off documented in
//     PlatformController::changeDomain (server side).
//  2. Apply the new route list via the same Caddy.ApplyDeploymentRoutes
//     path the deploy handler uses. Caddy reload is zero-downtime;
//     Let's Encrypt issues a fresh cert on the first hit to the new
//     hostname (within ~10s for a healthy zone).
//  3. Onion identity is preserved by re-injecting the existing
//     OnionAddr passed in the payload — the Tor manager is NOT
//     re-invoked, so the same .onion keeps publishing.
//  4. Phase 89: when the payload's ProvisionOnion is true AND no
//     OnionAddr is supplied, the agent provisions a new hidden
//     service first (cold-starts Tor if needed) and reports the
//     published address back via DeployResult.Onion. Server uses
//     this to back-fill imprezaplatform_deployments.onion when the
//     customer added .onion to an app that was running clearnet-only.
func (d *Docker) updateRoutes(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.UpdateRoutesPayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode update_routes payload: "+err.Error())
	}
	if p.DeploymentID == "" {
		return failResult(cmd.ID, "update_routes payload missing deployment_id")
	}
	appDir := d.appDir(p.DeploymentID)
	if !exists(appDir) {
		return failResult(cmd.ID, "no state dir for deployment "+p.DeploymentID)
	}

	// Phase 89 — post-deploy onion provisioning. When the server asks
	// us to add a hidden service to a deployment that doesn't have one
	// yet (the customer clicked "+ Add .onion" on an app already
	// running with clearnet only), spin up Tor + publish before the
	// Caddy route build below — the routes will reference the freshly-
	// minted OnionAddr inline. Idempotent on OnionAddr already being
	// set: server should only ship ProvisionOnion=true when the
	// deployment's onion column is empty, but if a stale command sneaks
	// through (e.g. retry after a partial failure) we noop instead of
	// rotating keys.
	if p.ProvisionOnion && p.OnionAddr == "" && d.Tor != nil {
		provCtx, cancelProv := context.WithTimeout(ctx, 90*time.Second)
		addr, err := d.Tor.ProvisionHiddenService(provCtx, p.DeploymentID, 80)
		cancelProv()
		if err != nil {
			return failResult(cmd.ID, "provision hidden service: "+err.Error())
		}
		p.OnionAddr = addr
		d.Log.Info("update_routes: provisioned onion",
			"deployment_id", p.DeploymentID, "onion", addr)
	}

	// 1. Atomic .env rewrite. Skipped when the server passes no vars
	//    (a future caller may want to mutate routes without
	//    touching env, e.g. flipping TLS mode).
	if len(p.Vars) > 0 {
		if err := writeAtomic(filepath.Join(appDir, ".env"), []byte(renderEnv(p.Vars)), 0o600); err != nil {
			return failResult(cmd.ID, "write .env: "+err.Error())
		}
	}

	// 2. Re-apply Caddy routes. Same shape conversion the deploy
	//    handler does — kept verbatim so a future Phase 9.4-style
	//    routing change lands in one place.
	if d.Proxy != nil {
		// Ensure network + Caddy container are up before reload —
		// no-op if they already are, but defensive when the agent
		// has been restarted between deploy and update_routes.
		netCtx, cancelNet := context.WithTimeout(ctx, composeQueryTimeout)
		if err := d.Proxy.EnsureNetwork(netCtx); err != nil {
			cancelNet()
			return failResult(cmd.ID, "ensure proxy network: "+err.Error())
		}
		cancelNet()

		// Phase 9.11d v2: credentials are owned by the agent process
		// (cmd/run.go calls SetImprezaCredentials at startup), not
		// re-shipped from the server per-command. Nothing to do here.

		if len(p.Routes) > 0 {
			runCtx, cancelRun := context.WithTimeout(ctx, composeUpTimeout)
			if err := d.Proxy.EnsureRunning(runCtx); err != nil {
				cancelRun()
				return failResult(cmd.ID, "ensure caddy running: "+err.Error())
			}
			cancelRun()
		}

		routes := make([]proxy.Route, 0, len(p.Routes))
		for _, r := range p.Routes {
			upstream := r.Upstream
			if upstream == "" {
				upstream = fmt.Sprintf("dpl_%s:%d", p.DeploymentID, r.TargetPort)
			}
			mode := "letsencrypt"
			email := ""
			dnsProvider := ""
			if r.TLS != nil {
				if r.TLS.Mode != "" {
					mode = r.TLS.Mode
				}
				email = r.TLS.Email
				dnsProvider = r.TLS.DNSProvider
			}
			onion := ""
			if r.Onion != nil && r.Onion.Enabled && p.OnionAddr != "" {
				onion = p.OnionAddr
			}
			routes = append(routes, proxy.Route{
				Hostname:       r.Hostname,
				OnionAddr:      onion,
				Upstream:       upstream,
				TLSMode:        mode,
				TLSEmail:       email,
				TLSDNSProvider: dnsProvider,
			})
		}
		applyCtx, cancelApply := context.WithTimeout(ctx, composeQueryTimeout)
		defer cancelApply()
		if err := d.Proxy.ApplyDeploymentRoutes(applyCtx, p.DeploymentID, routes); err != nil {
			return failResult(cmd.ID, "apply caddy routes: "+err.Error())
		}
	}

	d.Log.Info("docker update_routes: success",
		"deployment_id", p.DeploymentID,
		"routes", len(p.Routes),
		"onion", p.OnionAddr != "")
	return sdkclient.DeployResult{
		CommandID:    cmd.ID,
		Status:       "success",
		DeploymentID: p.DeploymentID,
		Domain:       envValue(p.Vars, "DOMAIN_URL"),
		Onion:        p.OnionAddr,
	}
}

// ─────────────────────────────────────────────────────────────────────
// logsTail
// ─────────────────────────────────────────────────────────────────────

// Logs-tail chunks are capped at 200 KB so a single chunk fits well
// within the server's 256 KB per-chunk limit even after JSON encoding
// overhead.
const logsChunkSize = 200 * 1024

// logsTail runs `docker compose logs --tail N --since Ts` for the
// deployment, splits the output into chunks, and ships them back via
// the SDK client. The DeployResult itself is just a success/failure
// signal — the actual log content rides on /v1/agent/logs.
func (d *Docker) logsTail(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.LogsTailPayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode logs_tail payload: "+err.Error())
	}
	if p.DeploymentID == "" {
		return failResult(cmd.ID, "logs_tail payload missing deployment_id")
	}
	if p.StreamID == "" {
		return failResult(cmd.ID, "logs_tail payload missing stream_id")
	}

	appDir := d.appDir(p.DeploymentID)
	if !exists(appDir) {
		// Send a single final chunk explaining the no-op so the
		// caller's GET .../logs returns *something* rather than
		// silently timing out.
		d.shipChunk(ctx, p, "logs_tail: no state dir for deployment (no-op).\n", true)
		return sdkclient.DeployResult{
			CommandID:    cmd.ID,
			Status:       "success",
			DeploymentID: p.DeploymentID,
		}
	}

	// Build `docker compose logs ...` args. We use `--no-color` so the
	// caller sees raw text without ANSI escapes.
	args := []string{"logs", "--no-color"}
	if p.SinceSeconds > 0 {
		args = append(args, "--since", fmt.Sprintf("%ds", p.SinceSeconds))
	} else {
		// Read `lines` from the payload top-level. SDK's LogsTailPayload
		// doesn't have it as a named field (planned for v2), so we
		// fall back to decoding the raw payload as a map for that field.
		lines := extractLines(cmd.Payload)
		if lines <= 0 {
			lines = 200
		}
		args = append(args, "--tail", fmt.Sprintf("%d", lines))
	}

	logCtx, cancelLog := context.WithTimeout(ctx, 30*time.Second)
	defer cancelLog()
	out, err := d.compose(logCtx, appDir, args...)
	if err != nil {
		// Include the stderr in the chunk so callers can debug.
		msg := fmt.Sprintf("logs_tail: docker compose logs failed: %v\n%s", err, tail(out, 4096))
		d.shipChunk(ctx, p, msg, true)
		return failResult(cmd.ID, msg)
	}

	// Split the output into chunks ≤ logsChunkSize, ship each in turn.
	body := string(out)
	if body == "" {
		d.shipChunk(ctx, p, "logs_tail: (no log output)\n", true)
		return sdkclient.DeployResult{
			CommandID:    cmd.ID,
			Status:       "success",
			DeploymentID: p.DeploymentID,
		}
	}
	for off := 0; off < len(body); {
		end := off + logsChunkSize
		if end > len(body) {
			end = len(body)
		}
		final := end == len(body)
		if err := d.shipChunk(ctx, p, body[off:end], final); err != nil {
			d.Log.Warn("logs_tail: ship chunk failed", "stream_id", p.StreamID, "err", err)
			return failResult(cmd.ID, "ship log chunk: "+err.Error())
		}
		off = end
	}

	return sdkclient.DeployResult{
		CommandID:    cmd.ID,
		Status:       "success",
		DeploymentID: p.DeploymentID,
	}
}

// shipChunk posts one log chunk back to the control plane. Returns
// nil + logs a warning when no client is wired (test mode).
func (d *Docker) shipChunk(ctx context.Context, p sdkclient.LogsTailPayload, chunk string, final bool) error {
	if d.Client == nil {
		d.Log.Warn("logs_tail: no SDK client wired, dropping chunk",
			"stream_id", p.StreamID, "bytes", len(chunk), "final", final)
		return nil
	}
	postCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return d.Client.AgentLogs(postCtx, sdkclient.LogChunk{
		StreamID: p.StreamID,
		Chunk:    chunk,
		Final:    final,
	})
}

// extractLines pulls the optional `lines` field out of a raw
// LogsTailPayload. Kept separate from the SDK struct so we can add it
// to v2 without a breaking change.
func extractLines(payload []byte) int {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return 0
	}
	if v, ok := m["lines"]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			i, _ := strconv.ParseInt(n, 10, 32)
			return int(i)
		}
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

// appDir returns the canonical per-deployment state path.
func (d *Docker) appDir(deploymentID string) string {
	return filepath.Join(d.StateDir, "apps", deploymentID)
}

// compose runs `docker compose <args>` with appDir as the working
// directory. Combined stdout + stderr is returned so the caller can
// attach a tail to the result on failure.
func (d *Docker) compose(ctx context.Context, appDir string, args ...string) ([]byte, error) {
	full := append([]string{"compose"}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = appDir
	// `docker compose` reads .env automatically when present in the
	// working directory — no extra flag needed.
	//
	// Phase 12 Iteration 3a: the docker CLI tries to create
	// $HOME/.docker (default /root/.docker on systemd-run agents)
	// for buildx state when `docker compose up` invokes `docker build`
	// for a Dockerfile-mode deploy. Our systemd unit hardens
	// ProtectHome=yes, which makes /root read-only and breaks the
	// build with "mkdir /root/.docker: read-only file system".
	//
	// Force DOCKER_CONFIG into the agent's state dir — that's the
	// one writable location we always own. Inherits the rest of the
	// process env so PATH + everything else keeps working.
	cmd.Env = append(os.Environ(),
		"DOCKER_CONFIG="+filepath.Join(d.StateDir, ".docker"),
	)
	return cmd.CombinedOutput()
}

// writeAtomic writes data to a temp file in the destination directory,
// fsyncs + closes, then renames into place. Avoids leaving a
// half-written .env behind on crash mid-write.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".impreza-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// renderEnv emits sorted KEY=VALUE pairs as a docker-compose-compatible
// .env file. Keys are sorted so re-renders produce identical output (so
// `docker compose` doesn't see a "changed env" between identical deploys).
//
// Values are stringified via fmt.Sprint and newline-escaped. We do NOT
// quote values — docker-compose `.env` format treats `KEY=value with
// spaces` correctly as long as there are no `#` mid-line. For values
// containing `#` or backslashes, the manifest author is expected to
// keep them out of vars (they'd be in compose_yaml instead).
func renderEnv(vars map[string]any) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		v := fmt.Sprintf("%v", vars[k])
		v = strings.ReplaceAll(v, "\r", "")
		v = strings.ReplaceAll(v, "\n", "\\n")
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(v)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// envValue returns vars[key] as a string, or "" when missing.
func envValue(vars map[string]any, key string) string {
	if v, ok := vars[key]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// runShellScript executes the given shell text via /bin/sh -c in
// appDir with `vars` exposed as env vars on top of the agent's own
// environment. Used by the Phase 9.9 lifecycle.install hook to drive
// app-specific wizard completion.
//
// Normalizes Windows CRLF → LF before exec. The manifests live in a
// PHP file that operators edit on Windows boxes, and JSON encoding
// preserves the CRs verbatim — /bin/sh then chokes on `do\r`
// expecting `do`. Strip them defensively so manifest authors don't
// have to think about line endings.
func (d *Docker) runShellScript(ctx context.Context, appDir string, vars map[string]any, script string) ([]byte, error) {
	script = strings.ReplaceAll(script, "\r\n", "\n")
	script = strings.ReplaceAll(script, "\r", "\n")
	c := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	c.Dir = appDir
	c.Env = os.Environ()
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic env ordering for log reproducibility
	for _, k := range keys {
		c.Env = append(c.Env, fmt.Sprintf("%s=%v", k, vars[k]))
	}
	return c.CombinedOutput()
}

// applyDataDirConfig honors manifest.runtime.data_dir on the bind-mount
// directory the agent just created. Mode is an octal string ("0755",
// "750", etc.); Owner is "uid:gid" or "uid" (both numeric). Either is
// optional — pass an empty string to skip that step.
//
// The chmod is non-recursive on purpose: most images create their own
// subdirs with the correct mode during entrypoint, so we only need to
// widen the top-level /data so the dropped-to user can traverse it.
//
// chown IS recursive — apps that need their data tree fully owned by
// the image's native uid (Gitea uid 1000, Nextcloud's www-data 33,
// Synapse 991, ...) want every existing file to flip ownership on a
// re-deploy too, not just newly created ones.
func applyDataDirConfig(dataDir string, dd *sdkclient.DataDirConfig) error {
	if dd == nil {
		return nil
	}
	if dd.Mode != "" {
		mode, err := strconv.ParseUint(dd.Mode, 8, 32)
		if err != nil {
			return fmt.Errorf("invalid data_dir.mode %q (expected octal like 0755): %w", dd.Mode, err)
		}
		if err := os.Chmod(dataDir, os.FileMode(mode)); err != nil {
			return fmt.Errorf("chmod %s: %w", dataDir, err)
		}
	}
	if dd.Owner != "" {
		uid, gid, err := parseOwner(dd.Owner)
		if err != nil {
			return fmt.Errorf("invalid data_dir.owner %q (expected uid or uid:gid): %w", dd.Owner, err)
		}
		walkErr := filepath.WalkDir(dataDir, func(path string, _ os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			return os.Chown(path, uid, gid)
		})
		if walkErr != nil {
			return fmt.Errorf("chown -R %s to %d:%d: %w", dataDir, uid, gid, walkErr)
		}
	}
	return nil
}

// parseOwner accepts "uid", "uid:gid", or "uid:" (gid defaults to uid).
// Both fields are numeric — we never resolve names since the container's
// user table won't match the host's.
func parseOwner(s string) (uid, gid int, err error) {
	parts := strings.SplitN(s, ":", 2)
	uid64, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	uid = int(uid64)
	if len(parts) < 2 || parts[1] == "" {
		return uid, uid, nil
	}
	gid64, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	return uid, int(gid64), nil
}

// exists reports whether the given path exists. Errors other than
// ErrNotExist are treated as "yes" because we'd rather try and fail
// loudly than silently skip cleanup.
func exists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	return !errors.Is(err, os.ErrNotExist)
}

// tail returns up to n bytes from the end of data, with a truncation
// marker when the original is longer. Used to keep DeployResult bodies
// small while preserving the actually-useful "last few lines" of logs.
func tail(data []byte, n int) string {
	s := string(data)
	if len(s) <= n {
		return s
	}
	return "...(truncated)\n" + s[len(s)-n:]
}

// failResult is a shorthand for "command finished, status=failed".
func failResult(commandID, msg string) sdkclient.DeployResult {
	return sdkclient.DeployResult{
		CommandID: commandID,
		Status:    "failed",
		Error:     msg,
	}
}

// ─────────────────────────────────────────────────────────────────────
// Phase 12 Iteration 3 — Dockerfile-mode build context download
// ─────────────────────────────────────────────────────────────────────

// fetchAndExtractBuildContext downloads a Phase 12 build context tarball
// from the control plane, verifies its SHA256 against the manifest, and
// extracts it into destDir. Caller (deploy()) gates on Build != nil +
// Client != nil before invoking.
//
// The URL field accepts both a full https:// URL and a control-plane-
// relative path (starting with "/"). For relative paths the agent's
// own Client.GetRaw uses its configured BaseURL; for absolute URLs the
// path is computed by stripping the scheme+host and we still GET via
// the agent client so auth headers attach. Mixed-host fetches (an
// absolute URL pointing somewhere other than BaseURL) are rejected to
// keep the agent from being tricked into shipping its agent_secret to
// an attacker-controlled host via a tampered manifest.
//
// Disk safety:
//   - destDir is removed + recreated fresh on every call (a previous
//     failed extract leaves no partial files in the way).
//   - Each tar entry's destination is path-cleaned + bounded to destDir
//     to prevent path-traversal via crafted ../../ entries.
//   - Symlinks pointing outside destDir are rejected outright (defense
//     against the "symlink to /etc/passwd then write through it on a
//     subsequent extract" class of attacks; agent doesn't run as root
//     in production but defense in depth is cheap here).
//   - Entries larger than 256 MiB individually fail the extract — the
//     server enforces a 100 MB cap on the whole tarball so this is
//     more about catching corruption than an actual attack vector.
func fetchAndExtractBuildContext(
	ctx context.Context,
	cli *sdkclient.Client,
	log *slog.Logger,
	bc *sdkclient.BuildContext,
	destDir string,
	gitAuthMethod string,
	deploymentID string,
) error {
	if bc == nil {
		return errors.New("build context is nil")
	}
	// Phase 15 — git-clone path takes precedence when set. The agent
	// shallow-clones the repo at the requested ref directly into
	// destDir + skips the tarball-download path entirely. URL + SHA256
	// (the tarball fields) are ignored when Git is populated; the
	// server enforces "pick exactly one" upstream so both being set
	// is operator misconfiguration we tolerate by preferring Git.
	if bc.Git != nil && bc.Git.URL != "" {
		return gitCloneIntoBuildContext(ctx, cli, log, bc.Git, destDir, gitAuthMethod, deploymentID)
	}

	// Tarball path (Phase 12.3a Dockerfile-via-context-upload). Both
	// URL + SHA256 are required for the integrity gate.
	if bc.URL == "" {
		return errors.New("build context has empty URL (and no .git)")
	}
	if bc.SHA256 == "" {
		return errors.New("build context missing sha256 (refusing to download unauthenticated bytes)")
	}

	// Resolve the request path. Anything starting with "/" is treated
	// as control-plane-relative; full URLs must point to the same host
	// as the agent's configured BaseURL.
	reqPath := bc.URL
	if !strings.HasPrefix(reqPath, "/") {
		u, err := url.Parse(reqPath)
		if err != nil {
			return fmt.Errorf("parse build context URL: %w", err)
		}
		base, err := url.Parse(cli.BaseURL)
		if err != nil {
			return fmt.Errorf("parse client base URL: %w", err)
		}
		if u.Scheme != base.Scheme || u.Host != base.Host {
			return fmt.Errorf("build context URL host %q does not match agent base %q (refusing cross-host fetch)", u.Host, base.Host)
		}
		reqPath = u.RequestURI()
	}

	log.Info("docker deploy: fetching build context",
		"url", reqPath,
		"expected_sha256", bc.SHA256,
		"expected_size", bc.SizeBytes,
	)

	// 5-minute budget on the download — 100 MB tarball over a typical
	// VPS uplink is well under that even with overhead.
	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	body, err := cli.GetRaw(dlCtx, reqPath, nil)
	if err != nil {
		return fmt.Errorf("download tarball: %w", err)
	}
	if bc.SizeBytes > 0 && int64(len(body)) != bc.SizeBytes {
		return fmt.Errorf("tarball size mismatch: got %d, expected %d", len(body), bc.SizeBytes)
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, bc.SHA256) {
		return fmt.Errorf("tarball sha256 mismatch: got %s, expected %s", got, bc.SHA256)
	}

	if err := os.RemoveAll(destDir); err != nil {
		return fmt.Errorf("clear previous build context: %w", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir build context: %w", err)
	}

	gz, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	const maxEntryBytes = 256 * 1024 * 1024 // 256 MiB per entry
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("abs destDir: %w", err)
	}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		// Path-traversal guard: clean + ensure the destination stays
		// inside destAbs. Strip leading "./" so well-formed entries
		// from `tar c -C dir .` still extract cleanly.
		cleanName := filepath.Clean("/" + hdr.Name)
		cleanName = strings.TrimPrefix(cleanName, string(filepath.Separator))
		target := filepath.Join(destAbs, cleanName)
		if !strings.HasPrefix(target, destAbs+string(filepath.Separator)) && target != destAbs {
			return fmt.Errorf("tar entry %q escapes destination dir", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs0755(hdr.Mode)); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size > maxEntryBytes {
				return fmt.Errorf("tar entry %q exceeds %d bytes", hdr.Name, maxEntryBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %q: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs0644(hdr.Mode))
			if err != nil {
				return fmt.Errorf("open %q: %w", target, err)
			}
			if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
				f.Close()
				return fmt.Errorf("write %q: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %q: %w", target, err)
			}
		case tar.TypeSymlink:
			// Reject symlinks pointing outside destAbs. We resolve the
			// link's target against the entry's parent dir to mimic the
			// kernel's lookup semantics — same containment check.
			linkAbs := hdr.Linkname
			if !filepath.IsAbs(linkAbs) {
				linkAbs = filepath.Join(filepath.Dir(target), hdr.Linkname)
			}
			linkClean := filepath.Clean(linkAbs)
			if !strings.HasPrefix(linkClean, destAbs+string(filepath.Separator)) && linkClean != destAbs {
				return fmt.Errorf("tar symlink %q -> %q escapes destination dir", hdr.Name, hdr.Linkname)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %q: %w", target, err)
			}
		default:
			// Hardlinks, char/block devices, FIFOs — skip silently.
			// Customer project tarballs shouldn't contain these; if
			// they do, ignoring is safer than honoring.
		}
	}

	return nil
}

// validGitRef mirrors the server's PlatformController::validateGitRef.
// Defense-in-depth (HARDENING.md #2): the control plane already rejects
// malformed refs at create time, but this keeps the agent self-defending
// if a future code path hands it an unvalidated ref.
var validGitRef = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// gitCloneIntoBuildContext shallow-clones a git repository into destDir
// for Dockerfile-mode-via-git deploys. git (and, for deploy-key auth,
// ssh) must be installed on the VPS — every Debian/Ubuntu base has both;
// if missing, the clone surfaces a clear error.
//
// We use --depth=1 + --single-branch + --no-tags to keep the clone tiny —
// only the head tree of the target ref is needed for `docker build`.
//
// Auth (authMethod, from the deploy payload):
//   - "" / "none" → public clone over https, no credential.
//   - "deploy_key" → SSH clone. The private key is fetched JIT from the
//     control plane, written to a 0600 temp file, used via GIT_SSH_COMMAND,
//     then shredded + removed in a defer. It never persists on disk.
//   - "pat" → https clone with a git credential helper fed the token via
//     the IMPREZA_GIT_TOKEN env var — never in argv (ps-visible) or the URL.
//
// Path-traversal guarantee: git sandboxes its own output under destDir
// (always a fresh empty dir). A malicious repo tree with an outbound
// symlink can pollute the docker BUILD env but not write to the host;
// sufficient for single-tenant anti-abuse (trust the customer + cgroups).
func gitCloneIntoBuildContext(
	ctx context.Context,
	cli *sdkclient.Client,
	log *slog.Logger,
	g *sdkclient.BuildContextGit,
	destDir string,
	authMethod string,
	deploymentID string,
) error {
	if g == nil || g.URL == "" {
		return errors.New("git build context has empty URL")
	}

	ref := g.Ref
	if ref == "" {
		ref = "main"
	}
	if strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || !validGitRef.MatchString(ref) {
		return fmt.Errorf("invalid git ref %q (letters, digits, '.', '_', '-', '/'; no leading '-' or '..')", ref)
	}

	// Resolve the credential for private modes — fetched just-in-time,
	// used transiently below, never written to deployment state.
	var credential string
	switch authMethod {
	case "", "none":
		// Public clone — https only (mirrors the server validator).
		if !strings.HasPrefix(g.URL, "https://") {
			return fmt.Errorf("git URL must start with https:// for a public clone (got: %q)", g.URL)
		}
	case "deploy_key", "pat":
		if cli == nil {
			return errors.New("private git auth requires an SDK client")
		}
		if deploymentID == "" {
			return errors.New("private git auth requires a deployment id")
		}
		credCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cred, err := cli.AgentGitCredential(credCtx, deploymentID)
		cancel()
		if err != nil {
			return fmt.Errorf("fetch git credential: %w", err)
		}
		if cred.Method != authMethod {
			return fmt.Errorf("git credential method mismatch (payload %q, server %q)", authMethod, cred.Method)
		}
		if cred.Credential == "" {
			return errors.New("git credential is empty")
		}
		credential = cred.Credential
	default:
		return fmt.Errorf("unknown git_auth_method %q", authMethod)
	}

	// Reset destDir so a partial previous clone doesn't poison the build.
	if err := os.RemoveAll(destDir); err != nil {
		return fmt.Errorf("clear previous build context: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("mkdir build context parent: %w", err)
	}

	log.Info("docker deploy: cloning git build context",
		"url", g.URL,
		"ref", ref,
		"auth", authMethodLabel(authMethod),
		"expected_commit", g.CommitSHA,
	)

	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Minimal base env — no HOME (.gitconfig), no SSH_AUTH_SOCK, refuse
	// interactive prompts. Auth-specific entries are appended per method.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_HTTP_LOW_SPEED_LIMIT=1024",
		"GIT_HTTP_LOW_SPEED_TIME=60",
	}
	cloneArgs := []string{
		"clone", "--depth=1", "--single-branch", "--no-tags",
		"--branch", ref, "--", g.URL, destDir,
	}
	var preArgs []string // git-level `-c` options, before the `clone` verb

	switch authMethod {
	case "deploy_key":
		// Write the fetched private key to a 0600 temp file and point ssh
		// at it; PrivateTmp=true on the systemd unit already isolates /tmp.
		keyFile, err := os.CreateTemp("", "impreza-deploykey-*")
		if err != nil {
			return fmt.Errorf("create deploy-key temp file: %w", err)
		}
		keyPath := keyFile.Name()
		defer shredFile(keyPath)
		keyData := credential
		if !strings.HasSuffix(keyData, "\n") {
			keyData += "\n" // ssh rejects a key file without a trailing newline
		}
		if _, err := keyFile.WriteString(keyData); err != nil {
			keyFile.Close()
			return fmt.Errorf("write deploy key: %w", err)
		}
		if err := keyFile.Close(); err != nil {
			return fmt.Errorf("close deploy key: %w", err)
		}
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return fmt.Errorf("chmod deploy key: %w", err)
		}
		// accept-new pins the host key on first use without prompting;
		// the known-hosts file is /dev/null so nothing persists.
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+keyPath+
			" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"+
			" -o UserKnownHostsFile=/dev/null -o PasswordAuthentication=no -o BatchMode=yes")
	case "pat":
		if !strings.HasPrefix(g.URL, "https://") {
			return fmt.Errorf("git URL must start with https:// for a PAT clone (got: %q)", g.URL)
		}
		// The credential helper reads the token from the env, so it never
		// appears in argv or the URL. The empty `credential.helper=` first
		// resets any inherited system/global helper chain.
		env = append(env, "IMPREZA_GIT_TOKEN="+credential)
		preArgs = []string{
			"-c", "credential.helper=",
			"-c", `credential.helper=!f(){ echo username=x; echo "password=$IMPREZA_GIT_TOKEN"; };f`,
		}
	}

	cmd := exec.CommandContext(cloneCtx, "git", append(preArgs, cloneArgs...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone %s @ %s: %w\n%s", g.URL, ref, err, tail(out, 1024))
	}

	// Optional CommitSHA verification — Iteration B will harden this.
	// For now log a discrepancy as a warning so operators have a
	// breadcrumb if a force-push between webhook + agent-poll sneaks
	// a different head in.
	if g.CommitSHA != "" {
		revCtx, revCancel := context.WithTimeout(ctx, 10*time.Second)
		revCmd := exec.CommandContext(revCtx, "git", "rev-parse", "HEAD")
		revCmd.Dir = destDir
		revCmd.Env = cmd.Env
		revOut, revErr := revCmd.Output()
		revCancel()
		if revErr == nil {
			gotSha := strings.TrimSpace(string(revOut))
			if !strings.EqualFold(gotSha, g.CommitSHA) {
				log.Warn("docker deploy: cloned commit differs from expected",
					"expected", g.CommitSHA,
					"got", gotSha,
				)
			}
		}
	}

	return nil
}

// authMethodLabel renders a log-safe label for the git auth method so the
// logs record which path ran without ever logging the credential.
func authMethodLabel(m string) string {
	switch m {
	case "deploy_key":
		return "deploy_key(ssh)"
	case "pat":
		return "pat(https)"
	default:
		return "public"
	}
}

// shredFile best-effort overwrites a small secret file with zeros, then
// removes it. Not a forensic guarantee on a journaling FS / SSD, but it
// closes the window where a plaintext deploy key sits readable on disk;
// the file also lives under the unit's PrivateTmp.
func shredFile(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		if f, err := os.OpenFile(path, os.O_WRONLY, 0o600); err == nil {
			zeros := make([]byte, fi.Size())
			_, _ = f.WriteAt(zeros, 0)
			_ = f.Sync()
			_ = f.Close()
		}
	}
	_ = os.Remove(path)
}

// fs0755 / fs0644 produce a safe FileMode from the int64 mode field
// in a tar header. tar mode bits can be arbitrary (including 0); fall
// back to a sensible default rather than letting a quirky archive
// produce unreadable / world-writable files.
func fs0755(mode int64) os.FileMode {
	m := os.FileMode(mode) & os.ModePerm
	if m == 0 {
		return 0o755
	}
	return m
}
func fs0644(mode int64) os.FileMode {
	m := os.FileMode(mode) & os.ModePerm
	if m == 0 {
		return 0o644
	}
	return m
}
