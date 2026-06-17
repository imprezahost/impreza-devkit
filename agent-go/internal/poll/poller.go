// Package poll implements the agent's main loop: long-poll for a
// command, hand it to the executor, report the result, repeat. A
// parallel heartbeat goroutine keeps the agent visible to the panel
// even when no commands are flowing.
package poll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/sysload"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Poller owns the long-poll loop. Build one via New and call Run.
type Poller struct {
	cfg    *config.Config
	client *sdkclient.Client
	exec   executor.Executor
	log    *slog.Logger

	// Static metadata included in every heartbeat. Set at construction.
	agentVersion string
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

	return &Poller{
		cfg:          cfg,
		client:       c,
		exec:         exec,
		log:          log,
		agentVersion: agentVersion,
	}, nil
}

// Run blocks until ctx is cancelled, then returns nil. The poll loop
// is in the foreground; the heartbeat runs in a goroutine that exits
// when ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		p.heartbeatLoop(ctx)
	}()
	defer func() {
		// Wait for the heartbeat goroutine to wind down so caller
		// teardown (e.g. writing PID files, closing logs) sees a
		// truly idle agent.
		<-hbDone
	}()

	return p.pollLoop(ctx)
}

// pollLoop runs in the foreground. Returns nil when ctx is cancelled,
// or an error on a configuration problem the loop can't recover from
// (e.g. invalid base URL).
func (p *Poller) pollLoop(ctx context.Context) error {
	backoff := time.Duration(p.cfg.BackoffMinSeconds) * time.Second
	maxBackoff := time.Duration(p.cfg.BackoffMaxSeconds) * time.Second

	for {
		if ctx.Err() != nil {
			p.log.Info("poll loop: context cancelled, exiting")
			return nil
		}

		cmd, ok, err := p.client.AgentPoll(ctx, nil)
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

		if !ok {
			// 204 / empty — reconnect immediately.
			continue
		}

		p.log.Info("poll: received command", "id", cmd.ID, "kind", cmd.Kind)
		result := p.exec.Execute(ctx, cmd)

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

	// Future: report.RunningDeployments from the executor's
	// snapshot of state.

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
