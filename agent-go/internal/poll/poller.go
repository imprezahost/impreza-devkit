// Package poll implements the agent's main loop: long-poll for a
// command, hand it to the executor, report the result, repeat. A
// parallel heartbeat goroutine keeps the agent visible to the panel
// even when no commands are flowing.
package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/sysload"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/upgrade"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Poller owns the long-poll loop. Build one via New and call Run.
type Poller struct {
	// Runs under the operation lock after domain recovery and before fresh commands.
	BeforeCommands func(context.Context) error
	cfg            *config.Config
	client         *sdkclient.Client
	exec           executor.Executor
	log            *slog.Logger

	// Static metadata included in every heartbeat. Set at construction.
	agentVersion string
	journal      *commandJournal
	active       *commandRecord
	journalErr   error
}

// New constructs a Poller for the given config + executor. The SDK
// client is built once and reused — the underlying http.Client honors
// per-call context deadlines, so this is safe.
func New(cfg *config.Config, exec executor.Executor, agentVersion string, log *slog.Logger) (*Poller, error) {
	c, err := sdkclient.NewAgent(sdkclient.AgentOptions{
		AgentID:     cfg.AgentID,
		AgentSecret: cfg.AgentSecret,
		BaseURL:     cfg.ControlPlaneURL,
		UseTor:      cfg.UseTor,
		Proxy:       cfg.Proxy,
	})
	if err != nil {
		return nil, fmt.Errorf("build sdk client: %w", err)
	}
	// Wire the SDK client into the executor when it's the Docker
	// variant — used by the logs_tail handler to ship chunks back via
	// /v1/agent/logs. Echo + tests don't need it. Type-asserted so the
	// executor.Executor interface stays narrow.
	if docker, ok := exec.(*executor.Docker); ok {
		docker.Client = c
	}

	p := &Poller{
		cfg:          cfg,
		client:       c,
		exec:         exec,
		log:          log,
		agentVersion: agentVersion,
	}
	if docker, ok := exec.(*executor.Docker); ok {
		p.journal = &commandJournal{dir: filepath.Join(docker.StateDir, "operations")}
		docker.Progress = p.observeProgress
		docker.SavePreparation = p.savePreparation
		docker.SaveReplacement = p.saveReplacement
	}
	return p, nil
}

// Run blocks until ctx is cancelled, then returns nil. The poll loop
// is in the foreground; the heartbeat runs in a goroutine that exits
// when ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.journal != nil {
		lock, err := p.journal.open()
		if err != nil {
			return err
		}
		defer lock.Close()
		record, err := p.journal.load()
		if err != nil {
			return err
		}
		if record != nil && (record.AgentID != p.cfg.AgentID || record.ControlPlaneURL != p.cfg.ControlPlaneURL) {
			return errors.New("saved operation belongs to a different agent or control plane; reconciliation required")
		}
		p.active = record
	}
	if p.active != nil && p.active.DomainHandover != nil {
		if err := p.resumeRecord(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if p.active != nil {
			return errors.New("domain recovery remains unacknowledged")
		}
	}
	if p.BeforeCommands != nil {
		if err := p.BeforeCommands(ctx); err != nil {
			return err
		}
	}
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		p.heartbeatLoop(ctx)
	}()
	metricsDone := make(chan struct{})
	go func() {
		defer close(metricsDone)
		p.metricsLoop(ctx)
	}()
	defer func() {
		// Wait for the heartbeat goroutine to wind down so caller
		// teardown (e.g. writing PID files, closing logs) sees a
		// truly idle agent.
		cancel()
		<-hbDone
		<-metricsDone
	}()

	return p.pollLoop(ctx)
}

// pollLoop runs in the foreground. Returns nil when ctx is cancelled,
// or an error on a configuration problem the loop can't recover from
// (e.g. invalid base URL).
func (p *Poller) pollLoop(ctx context.Context) error {
	if p.active != nil {
		if err := p.resumeRecord(ctx); err != nil {
			return err
		}
	}
	capabilities := []string{executor.TorEgressProtocol, executor.TorDataOwnerProtocol, "onion-auth-v1", "onion-profile-v1", "onion-deploy-profile-v1", "onion-custody-v1", "onion-purge-v1", "onion-private-preview-v1", "startup-health-v1", "deploy-cancel-v1", "build-secrets-v1", "compose-source-files-v1", sdkclient.ServiceBindingProtocol, sdkclient.ServiceBindingRetirementProtocol, sdkclient.ServiceBindingGenerationProtocol, sdkclient.ServiceBindingGenerationRetirementProtocol, sdkclient.ServiceBindingRotationProtocol, sdkclient.ServiceBindingBackupProtocol, sdkclient.MysqlServiceBindingGenerationProtocol, sdkclient.MysqlServiceBindingGenerationRetirementProtocol, sdkclient.MysqlServiceBindingRotationProtocol, sdkclient.MysqlServiceBindingBackupProtocol, sdkclient.MysqlServiceBindingRestoreProtocol, sdkclient.TrafficSwitchProtocol, sdkclient.PreviewBasicAuthProtocol, sdkclient.ServiceBindingRestoreProtocol, sdkclient.ShieldProtocol, sdkclient.ProxyMetricsProtocol, sdkclient.SandboxProtocol}
	if p.journal != nil {
		capabilities = append(capabilities, sdkclient.DeploymentProgressProtocol)
		if _, ok := p.exec.(*executor.Docker); ok {
			capabilities = append(capabilities, sdkclient.HostFailoverFenceProtocol, sdkclient.DomainHandoverProtocol, sdkclient.OnionTransferProtocol, sdkclient.HostFailoverReleaseProtocol)
			if upgrade.Available() {
				capabilities = append(capabilities, upgrade.Protocol)
			}
		}
	}
	backoff := time.Duration(p.cfg.BackoffMinSeconds) * time.Second
	maxBackoff := time.Duration(p.cfg.BackoffMaxSeconds) * time.Second

	for {
		if ctx.Err() != nil {
			p.log.Info("poll loop: context cancelled, exiting")
			return nil
		}

		pollCapabilities := append([]string(nil), capabilities...)
		if docker, ok := p.exec.(*executor.Docker); ok {
			if enabled, err := docker.ControlledBuildsEnabled(); err == nil && enabled {
				pollCapabilities = append(pollCapabilities, executor.ControlledBuildProtocol)
			}
		}
		cmd, ok, err := p.client.AgentPoll(ctx, &sdkclient.PollRequest{Capabilities: pollCapabilities})
		if err != nil {
			// Distinguish auth from transport so we surface bad
			// credentials immediately instead of silently looping.
			var authErr *sdkclient.AuthError
			if errors.As(err, &authErr) {
				p.log.Error("poll: auth rejected — credentials may have been revoked", "err", err)
				return fmt.Errorf("agent credentials rejected by control plane: %w", err)
			}
			p.log.Warn("poll: transport error, backing off", "err", err, "backoff", backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		// Successful poll — reset the backoff window.
		backoff = time.Duration(p.cfg.BackoffMinSeconds) * time.Second

		// Enforce sandbox wall-clock budgets between commands.
		if reconciler, ok := p.exec.(interface {
			ReconcileSandbox(context.Context)
		}); ok {
			reconciler.ReconcileSandbox(ctx)
		}

		if !ok {
			// 204 / empty — reconnect immediately.
			continue
		}

		p.log.Info("poll: received command", "id", cmd.ID, "kind", cmd.Kind)
		if cmd.Kind == sdkclient.CommandHostFailoverFence &&
			(cmd.ProgressProtocol != sdkclient.DeploymentProgressProtocol || cmd.ControlToken == "" ||
				p.journal == nil || !validFencePayload(cmd.Payload)) {
			return errors.New("host failover fence requires a valid journaled command")
		}
		if cmd.ResumeOnly || cmd.ProgressProtocol != "" {
			if cmd.ProgressProtocol != sdkclient.DeploymentProgressProtocol || cmd.ControlToken == "" || p.journal == nil {
				return errors.New("unsupported operation recovery protocol")
			}
			p.active = &commandRecord{Version: 1, AgentID: p.cfg.AgentID, ControlPlaneURL: p.cfg.ControlPlaneURL, CommandID: cmd.ID, Kind: cmd.Kind, ControlToken: cmd.ControlToken, ProgressProtocol: cmd.ProgressProtocol, Step: "preparing"}
			if cmd.Kind == sdkclient.CommandHostFailoverFence {
				p.active.Payload = append([]byte(nil), cmd.Payload...)
			}

			if cmd.Kind == sdkclient.CommandUpdateRoutes {
				var route sdkclient.UpdateRoutesPayload
				if json.Unmarshal(cmd.Payload, &route) == nil && route.DomainHandover != nil {
					p.active.DomainHandover = &executor.DomainHandoverIdentity{DeploymentID: route.DeploymentID, Before: route.DomainHandover.Before, After: route.DomainHandover.After}
					if !p.active.DomainHandover.Valid() {
						return errors.New("invalid domain handover identity")
					}
				}
			}

			if !cmd.ResumeOnly && cmd.Kind == sdkclient.CommandDeploy {
				if _, ok := p.exec.(*executor.Docker); ok {
					p.active.Preparation = &executor.PreparationRecovery{Version: 1, Phase: "unstarted"}
				}
			}
			if err := p.journal.save(p.active); err != nil {
				return fmt.Errorf("persist operation before execution: %w", err)
			}
			if cmd.ResumeOnly {
				if err := p.resumeRecord(ctx); err != nil {
					return err
				}
				continue
			}
		}
		p.journalErr = nil
		result := p.exec.Execute(ctx, cmd)
		if p.journalErr != nil {
			return fmt.Errorf("preparation journal failed; execution stopped: %w", p.journalErr)
		}
		if p.active != nil && p.active.DomainHandover != nil && result.DomainHandover != nil && result.DomainHandover.Status == "recovery_required" {
			p.observeProgress(ctx, cmd, "interrupted")
			if err := p.resumeRecord(ctx); err != nil {
				return err
			}
			continue
		}
		if result.Status == executor.PreparationPendingStatus {
			if p.active == nil {
				return errors.New("supervised preparation lost its journal")
			}
			if err := p.resumeRecord(ctx); err != nil {
				return err
			}
			continue
		}
		result.ControlToken = cmd.ControlToken
		if p.active != nil {
			if result.CommandID != cmd.ID {
				return errors.New("executor returned a result for another command")
			}
			p.active.Result = &result
			if err := p.journal.save(p.active); err != nil {
				return fmt.Errorf("persist final result before reporting: %w", err)
			}
			p.observeProgress(ctx, cmd, "reporting_result")
			if err := p.sendSavedResult(ctx); err != nil {
				return err
			}
			continue
		}

		for cmd.ControlToken != "" && ctx.Err() == nil {
			if err := p.client.AgentDeployResult(ctx, result); err == nil {
				break
			} else {
				p.log.Warn("controlled deployment result awaiting acknowledgement", "command_id", cmd.ID, "err", err)
				if !sleepCtx(ctx, 5*time.Second) {
					return nil
				}
			}
		}
		if cmd.ControlToken != "" {
			continue
		}
		if err := p.client.AgentDeployResult(ctx, result); err != nil {
			// command_id is the idempotency key — the server will
			// tolerate a redelivery. Log and continue.
			p.log.Error("poll: report deploy-result failed", "command_id", cmd.ID, "err", err)
			continue
		}
		p.log.Info("poll: reported result", "command_id", cmd.ID, "status", result.Status)
	}
}

// heartbeatLoop runs in a goroutine and emits one heartbeat per
// HeartbeatSeconds. The control plane marks the agent offline after
// three consecutive misses (~90s at the default cadence).
func (p *Poller) heartbeatLoop(ctx context.Context) {
	interval := time.Duration(p.cfg.HeartbeatSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	// Send one immediately so the agent appears online on startup
	// without waiting for the first tick.
	p.sendHeartbeat(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sendHeartbeat(ctx)
		}
	}
}

// metricsLoop runs in a goroutine and emits one per-app metrics report per
// MetricsSeconds (default 60s). Numbers and container states only — the
// collector never reads environment, configuration or logs, so the report
// cannot carry a secret. Errors are logged and the next tick retries.
func (p *Poller) metricsLoop(ctx context.Context) {
	collector, ok := p.exec.(interface {
		CollectAppMetrics(context.Context) *sdkclient.AppMetricsReport
	})
	if !ok {
		return
	}
	interval := time.Duration(p.cfg.MetricsSeconds) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			report := collector.CollectAppMetrics(ctx)
			if report == nil || len(report.Apps) == 0 {
				continue
			}
			if err := p.client.AgentMetricsReport(ctx, *report); err != nil {
				p.log.Warn("metrics: report failed", "err", err)
			}
		}
	}
}

// sendHeartbeat builds and sends a single AgentReport. Errors are
// logged but never returned — heartbeat loss is recoverable and
// doesn't justify tearing down the daemon.
func (p *Poller) sendHeartbeat(ctx context.Context) {
	report := sdkclient.AgentReport{
		ReportedAt: time.Now().UTC(),
		Version:    p.agentVersion,
	}

	// Phase 9.23: ship the current resource snapshot so the control
	// plane can render live cards + decide whether to fire a
	// capacity-alert email. Errors are non-fatal; we just send the
	// heartbeat without Load. The sysload package is Linux-only —
	// dev builds on Windows/macOS get a stub that returns
	// ErrUnsupported, which we log once + suppress.
	if load, err := sysload.Collect(); err != nil {
		// Reduce log spam: warn once per hour. The Poller has no
		// throttle helper today, so we just log every heartbeat —
		// acceptable since prod runs only on Linux where Collect
		// succeeds. If a Linux read genuinely fails (unlikely),
		// repeated warns are actually useful signal.
		p.log.Warn("heartbeat: sysload.Collect failed", "err", err)
	} else if load != nil {
		report.Load = load
	}

	if collector, ok := p.exec.(interface {
		CollectRuntime(context.Context) *sdkclient.RuntimeSnapshot
	}); ok {
		report.Runtime = collector.CollectRuntime(ctx)
	}

	if err := p.client.AgentReport(ctx, report); err != nil {
		p.log.Warn("heartbeat: report failed", "err", err)
		return
	}
}

// nextBackoff doubles the current backoff up to max.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// sleepCtx sleeps for d unless ctx is cancelled first. Returns false
// if the sleep was interrupted by cancellation.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
