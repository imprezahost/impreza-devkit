package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/poll"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/state"
)

var runLogLevel string

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the long-poll loop (entry point of the systemd unit).",
	Long: `Run the long-poll loop.

Reads credentials from the config file written by bootstrap, opens a
persistent connection to the control plane, and serves incoming
commands until SIGTERM or SIGINT.

This is the entry point of the impreza-agent systemd unit; running it
manually is fine for dev / debugging.
`,
	RunE: runRun,
}

func init() {
	runCmd.Flags().StringVar(&runLogLevel, "log-level", "info",
		"Log level: debug | info | warn | error.")
}

func runRun(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(globalConfigPath)
	if err != nil {
		if errors.Is(err, config.ErrNoConfig) {
			return fmt.Errorf("no config — run `impreza-agent bootstrap --token bst_...` first")
		}
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	stateDir, err := state.Ensure("")
	if err != nil {
		return fmt.Errorf("ensure state dir: %w", err)
	}

	log := newLogger(runLogLevel)
	log.Info("agent starting",
		"agent_id", cfg.AgentID,
		"control_plane_url", cfg.ControlPlaneURL,
		"use_tor", cfg.UseTor,
		"version", version,
		"state_dir", stateDir,
	)

	// Docker is the production executor. Unknown / not-yet-implemented
	// command kinds (Update, Rollback, LogsTail, AgentUpgrade) fall
	// back to Echo inside Docker.Execute so the command queue advances
	// instead of getting stuck on a job the agent can't handle yet.
	exec := executor.NewDocker(stateDir, log)

	// Phase 9.11d v2: hand the agent's own credentials to the Caddy
	// sidecar's env-file. The bundled caddy-dns-impreza plugin uses
	// them to authenticate against the public API for ACME DNS-01
	// present/cleanup. No-op write when content already matches (the
	// typical case after the first launch). If the credentials change
	// — e.g. the agent gets re-bootstrapped — the env-file is rewritten
	// + Caddy restarted to pick up the new values.
	if exec.Proxy != nil {
		credCtx, credCancel := context.WithTimeout(cmd.Context(), 15*time.Second)
		if err := exec.Proxy.SetImprezaCredentials(credCtx, cfg.AgentID, cfg.AgentSecret, cfg.ControlPlaneURL); err != nil {
			log.Warn("could not seed caddy with impreza credentials — DNS-01 challenges will fail until next attempt",
				"err", err)
		}
		credCancel()
	}

	poller, err := poll.New(cfg, exec, version, log)
	if err != nil {
		return err
	}

	// Trap SIGTERM (systemd stop) + SIGINT (Ctrl-C) so the loop
	// shuts down cleanly. context.WithCancel makes the cancellation
	// observable everywhere in the loop.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigCh:
			log.Info("received signal, shutting down", "signal", sig.String())
			cancel()
		case <-ctx.Done():
		}
		// Stop notifying — second signal causes the default behavior
		// (immediate exit), which is what an impatient operator wants.
		signal.Stop(sigCh)
	}()

	if err := poller.Run(ctx); err != nil {
		log.Error("poll loop exited with error", "err", err)
		return err
	}
	log.Info("agent stopped cleanly")
	return nil
}

// newLogger returns a slog.Logger writing JSON to stdout. JSON because
// the systemd journal indexes structured fields; stdout because
// systemd captures it natively without extra config.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
