package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/state"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose config / network / credential issues.",
	Long: `Run a battery of checks to help an operator triage why the agent
isn't working. Each check prints a single-line result with a status
icon. Exit code is 0 only if every check passes.

Checks (in order):

  1. config file exists and parses
  2. agent_id + agent_secret present
  3. control plane DNS resolves
  4. Tor SOCKS port reachable (only when use_tor or proxy is set)
  5. credentials accepted by the control plane (single AgentReport call)
  6. state dir is writeable
`,
	RunE: runDoctor,
}

type check struct {
	name string
	fn   func(ctx context.Context) (string, error)
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	var cfg *config.Config
	checks := []check{
		{
			name: "config file readable",
			fn: func(_ context.Context) (string, error) {
				path := globalConfigPath
				if path == "" {
					path = config.DefaultPath()
				}
				c, err := config.Load(path)
				if err != nil {
					return "", err
				}
				cfg = c
				return path, nil
			},
		},
		{
			name: "credentials present",
			fn: func(_ context.Context) (string, error) {
				if cfg == nil {
					return "", errors.New("skipped (no config)")
				}
				if err := cfg.Validate(); err != nil {
					return "", err
				}
				return cfg.AgentID, nil
			},
		},
		{
			name: "control plane DNS resolves",
			fn: func(c context.Context) (string, error) {
				if cfg == nil {
					return "", errors.New("skipped (no config)")
				}
				u, err := url.Parse(cfg.ControlPlaneURL)
				if err != nil {
					return "", fmt.Errorf("parse control_plane_url: %w", err)
				}
				host := u.Hostname()
				ips, err := net.DefaultResolver.LookupIPAddr(c, host)
				if err != nil {
					return "", fmt.Errorf("resolve %s: %w", host, err)
				}
				return fmt.Sprintf("%s → %d address(es)", host, len(ips)), nil
			},
		},
		{
			name: "tor / proxy reachable",
			fn: func(c context.Context) (string, error) {
				if cfg == nil {
					return "", errors.New("skipped (no config)")
				}
				if !cfg.UseTor && cfg.Proxy == "" {
					return "not configured (skipped)", nil
				}
				addr := "127.0.0.1:9050"
				if cfg.Proxy != "" {
					u, err := url.Parse(cfg.Proxy)
					if err != nil {
						return "", fmt.Errorf("parse proxy URL: %w", err)
					}
					addr = u.Host
				}
				d := net.Dialer{Timeout: 5 * time.Second}
				conn, err := d.DialContext(c, "tcp", addr)
				if err != nil {
					return "", fmt.Errorf("dial %s: %w", addr, err)
				}
				_ = conn.Close()
				return addr, nil
			},
		},
		{
			name: "credentials accepted (heartbeat round-trip)",
			fn: func(c context.Context) (string, error) {
				if cfg == nil {
					return "", errors.New("skipped (no config)")
				}
				if err := cfg.Validate(); err != nil {
					return "", errors.New("skipped (credentials missing)")
				}
				cli, err := sdkclient.NewAgent(sdkclient.AgentOptions{
					AgentID:     cfg.AgentID,
					AgentSecret: cfg.AgentSecret,
					BaseURL:     cfg.ControlPlaneURL,
					UseTor:      cfg.UseTor,
					Proxy:       cfg.Proxy,
				})
				if err != nil {
					return "", fmt.Errorf("build sdk client: %w", err)
				}
				if err := cli.AgentReport(c, sdkclient.AgentReport{
					ReportedAt: time.Now().UTC(),
					Version:    version,
				}); err != nil {
					return "", err
				}
				return "OK", nil
			},
		},
		{
			name: "state directory writeable",
			fn: func(_ context.Context) (string, error) {
				dir, err := state.Ensure("")
				if err != nil {
					return dir, err
				}
				return dir, nil
			},
		},
	}

	pass, fail := 0, 0
	for _, c := range checks {
		detail, err := c.fn(ctx)
		if err != nil {
			fmt.Fprintf(os.Stdout, "  FAIL  %s — %v\n", c.name, err)
			fail++
			continue
		}
		if detail != "" {
			fmt.Fprintf(os.Stdout, "  ok    %s — %s\n", c.name, detail)
		} else {
			fmt.Fprintf(os.Stdout, "  ok    %s\n", c.name)
		}
		pass++
	}

	fmt.Println()
	fmt.Printf("%d passed, %d failed\n", pass, fail)
	if fail > 0 {
		return fmt.Errorf("%d check(s) failed", fail)
	}
	return nil
}
