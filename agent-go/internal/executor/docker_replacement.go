package executor

import (
	"context"
	"fmt"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"strings"
	"time"
)

// finishReplacement executes the already-authorized runtime phase once. Both
// synchronous deployments and detached workers share exactly this sequence.
func (d *Docker) finishReplacement(ctx context.Context, cmd *sdkclient.PollCommand, p sdkclient.DeployPayload, previousRelease *runtimeRelease, isRedeploy bool, primaryOnion string) sdkclient.DeployResult {
	appDir := d.appDir(p.DeploymentID)
	policy, err := resolveStartupPolicy(p.Manifest.Runtime.Startup)
	if err != nil {
		return failResult(cmd.ID, err.Error())
	}
	d.deploymentProgress(ctx, cmd, "replacing")
	// 2. Replace containers only after all image preparation succeeds.
	// Startup failures restore a verified previous release when available.
	// First installs and already-broken runtimes retain the teardown policy.
	// Images are ready. No build or registry access is allowed during the
	// replacement phase, including for customer-authored Compose manifests.
	upArgs := []string{"up", "-d", "--no-build", "--pull", "never"}
	upTimeout := composeUpTimeout
	d.Log.Info("docker deploy: bringing stack up",
		"deployment_id", p.DeploymentID, "build", p.Manifest.Runtime.Build != nil)
	upCtx, cancelUp := context.WithTimeout(ctx, upTimeout)
	defer cancelUp()
	failStartup := func(reason string) sdkclient.DeployResult {
		d.deploymentProgress(ctx, cmd, "recovering")
		r := d.recoverStartup(ctx, appDir, p.DeploymentID, previousRelease, isRedeploy, reason)
		r.CommandID = cmd.ID
		r.DeploymentID = p.DeploymentID
		return r
	}
	if out, err := d.compose(upCtx, appDir, upArgs...); err != nil {
		failLogs := d.grabFailureLogs(ctx, appDir)
		return failStartup(fmt.Sprintf(
			"docker compose up: %v\n%s\n%s", err, tail(out, 1536), failLogs,
		))
	}

	// 2.1 Settle gate. `up -d` exiting 0 means "containers created", not
	//     "app works" — see the settleVerdict docs. A crash-looping
	//     container is the one failure we can assert rather than guess,
	//     so it fails the deploy and invokes recovery or teardown: it cannot serve
	//     traffic, and leaving it costs the customer CPU and disk
	//     indefinitely (this is the dead-container-in-the-VPS case).
	//
	//     With a verified previous release, an unsettled replacement is a
	//     failure and triggers recovery. Without a recovery target, the
	//     legacy advisory policy remains unless healthy startup was required.
	d.deploymentProgress(ctx, cmd, "checking_health")
	settleStart := time.Now()
	verdict, detail := d.awaitStackSettledPolicy(ctx, p.DeploymentID, policy)
	switch verdict {
	case settleCrashLooping:
		d.Log.Warn("docker deploy: stack is crash-looping, tearing down",
			"deployment_id", p.DeploymentID, "detail", detail,
			"waited", time.Since(settleStart).Round(time.Second))
		failLogs := d.grabFailureLogs(ctx, appDir)
		return failStartup(fmt.Sprintf(
			"deploy did not produce a working app: %s\n%s", detail, failLogs,
		))
	case settleUnsettled:
		if previousRelease != nil || policy.RequireHealthy {
			return failStartup("new release did not pass startup checks before the deadline: " + detail + "\n" + d.grabFailureLogs(ctx, appDir))
		}
		d.Log.Warn("docker deploy: stack not confirmed healthy, proceeding anyway",
			"deployment_id", p.DeploymentID, "detail", detail,
			"waited", time.Since(settleStart).Round(time.Second))
	default:
		d.Log.Info("docker deploy: stack settled",
			"deployment_id", p.DeploymentID, "detail", detail,
			"waited", time.Since(settleStart).Round(time.Second))
	}
	settleNote := detail

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
		d.deploymentProgress(ctx, cmd, "installing")
		d.Log.Info("docker deploy: running lifecycle.install", "deployment_id", p.DeploymentID)
		instCtx, cancelInst := context.WithTimeout(ctx, 3*time.Minute)
		out, err := d.runShellScript(instCtx, appDir, p.Vars, installScript)
		cancelInst()
		if err != nil {
			return failStartup(fmt.Sprintf("lifecycle.install: %v\n%s", err, tail(out, 4096)))
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
		d.deploymentProgress(ctx, cmd, "routing")
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

	// 3.5 Manifest-declared readiness probe (manifest.lifecycle.health).
	//
	//     Until now this field was dead: the health_check command handler
	//     ran `compose ps` and always returned success without ever
	//     executing the script, so every manifest's carefully written
	//     probe did nothing. Running it here — last, after routes and the
	//     HTTPS handshake, so the app has had the most time to warm up —
	//     makes it live.
	//
	//     ADVISORY, like the HTTPS probe above. These scripts are
	//     app-specific curls that can fail for reasons that have nothing
	//     to do with a broken install (cold cache, slow migration, a
	//     probe written against an older version). The crash-loop gate is
	//     what fails a deploy; this only annotates the result.
	if healthScript := strings.TrimSpace(p.Manifest.Lifecycle.Health); healthScript != "" {
		d.deploymentProgress(ctx, cmd, "checking_readiness")
		hCtx, cancelH := context.WithTimeout(ctx, composeQueryTimeout)
		hOut, hErr := d.runShellScript(hCtx, appDir, p.Vars, healthScript)
		cancelH()
		if hErr != nil {
			d.Log.Warn("docker deploy: lifecycle.health probe did not pass (advisory)",
				"deployment_id", p.DeploymentID, "err", hErr)
			settleNote += fmt.Sprintf("\nreadiness probe did not pass (advisory): %v\n%s",
				hErr, tail(hOut, 512))
		} else {
			d.Log.Info("docker deploy: lifecycle.health probe passed",
				"deployment_id", p.DeploymentID)
			settleNote += "\nreadiness probe passed"
		}
	}

	// 4. Capture a short tail of logs for the result so the panel
	//    shows something concrete on success.
	logsCtx, cancelLogs := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancelLogs()
	logOut, _ := d.compose(logsCtx, appDir, "logs", "--tail=20", "--no-color")

	d.Log.Info("docker deploy: success", "deployment_id", p.DeploymentID, "onion", primaryOnion)
	var releaseMetadata *sdkclient.DeploymentRelease
	if verdict == settleHealthy {
		if release, err := d.captureRelease(ctx, appDir, p.DeploymentID); err != nil {
			settleNote += "\nRelease history could not be recorded: " + err.Error()
		} else if release != nil {
			releaseMetadata = &release.Metadata
		}
	}
	// Lead the tail with the settle/readiness summary. On an advisory
	// miss this is the only place an operator finds out the app was never
	// confirmed healthy, so it goes above the log tail rather than being
	// buried under 4KB of container output.
	return sdkclient.DeployResult{
		StartupCheck: policy.receipt(verdict),
		CommandID:    cmd.ID,
		Status:       "success",
		DeploymentID: p.DeploymentID,
		Domain:       envValue(p.Vars, "DOMAIN_URL"),
		Onion:        primaryOnion,
		Release:      releaseMetadata,
		LogsTail:     "health: " + settleNote + "\n\n" + tail(logOut, 4096),
	}
}
