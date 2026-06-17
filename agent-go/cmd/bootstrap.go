package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/state"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var (
	bootstrapToken           string
	bootstrapControlPlaneURL string
	bootstrapUseTor          bool
	bootstrapProxy           string
	bootstrapForce           bool
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Exchange a one-time token for permanent agent credentials.",
	Long: `Exchange a one-time bootstrap token for permanent agent credentials.

The token is single-use and expires within ~10 minutes of issuance. On
success the agent persists its credentials to the config file (default
/etc/impreza-agent/config.toml on Linux at 0600) and the server-side
token is invalidated immediately.

After bootstrap, start the long-poll loop:

    sudo systemctl enable --now impreza-agent

or directly:

    sudo impreza-agent run
`,
	RunE: runBootstrap,
}

func init() {
	bootstrapCmd.Flags().StringVar(&bootstrapToken, "token", "",
		"One-time bootstrap token (required). Env: IMPREZA_BOOTSTRAP.")
	bootstrapCmd.Flags().StringVar(&bootstrapControlPlaneURL, "control-plane", "",
		"Override the control-plane URL. Default: https://api.imprezahost.com.")
	bootstrapCmd.Flags().BoolVar(&bootstrapUseTor, "tor", false,
		"Route bootstrap traffic through the local Tor SOCKS port (127.0.0.1:9050).")
	bootstrapCmd.Flags().StringVar(&bootstrapProxy, "proxy", "",
		"SOCKS5 URL to route bootstrap traffic through (overrides --tor).")
	bootstrapCmd.Flags().BoolVar(&bootstrapForce, "force", false,
		"Overwrite an existing config file instead of erroring out.")
}

func runBootstrap(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	token := bootstrapToken
	if token == "" {
		token = os.Getenv("IMPREZA_BOOTSTRAP")
	}
	if token == "" {
		return fmt.Errorf("--token is required (or set IMPREZA_BOOTSTRAP)")
	}

	configPath := globalConfigPath
	if configPath == "" {
		configPath = config.DefaultPath()
	}

	if _, err := os.Stat(configPath); err == nil && !bootstrapForce {
		return fmt.Errorf("config already exists at %s — pass --force to overwrite", configPath)
	}

	hostname, _ := os.Hostname()
	req := sdkclient.BootstrapRequest{
		Hostname:     hostname,
		OS:           detectOS(),
		Arch:         runtime.GOARCH,
		AgentVersion: version,
	}

	resp, err := sdkclient.Bootstrap(ctx, token, req, sdkclient.BootstrapOptions{
		BaseURL: bootstrapControlPlaneURL,
		UseTor:  bootstrapUseTor,
		Proxy:   bootstrapProxy,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	cfg := &config.Config{
		AgentID:         resp.AgentID,
		AgentSecret:     resp.AgentSecret,
		ControlPlaneURL: resp.ControlPlaneURL,
		UseTor:          bootstrapUseTor,
		Proxy:           bootstrapProxy,
	}
	if err := cfg.Save(configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	// Ensure the state dir exists so the first run doesn't have to.
	stateDir, err := state.Ensure("")
	if err != nil {
		// Non-fatal — the agent will retry on first run.
		fmt.Fprintf(os.Stderr, "warn: could not create state dir %s: %v\n", stateDir, err)
	}

	fmt.Println("Bootstrap complete.")
	fmt.Printf("  agent_id:           %s\n", resp.AgentID)
	fmt.Printf("  control_plane_url:  %s\n", resp.ControlPlaneURL)
	fmt.Printf("  config:             %s\n", configPath)
	fmt.Println()
	fmt.Println("Start the agent with:")
	fmt.Println("  sudo systemctl enable --now impreza-agent")
	return nil
}

// detectOS returns a short identifier (e.g. "ubuntu-22.04", "debian-12",
// "darwin", "windows") for the BootstrapRequest. We avoid pulling in
// /etc/os-release parsing for the MVP — runtime.GOOS is good enough and
// the panel cares mostly about kernel-flavour, not distro-flavour.
func detectOS() string {
	// On Linux, try the cheapest reliable source.
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			return parseOSRelease(string(data))
		}
		return "linux"
	}
	return runtime.GOOS
}

// parseOSRelease pulls the canonical "ID-VERSION_ID" tuple out of an
// os-release file. Tolerant of quoted / unquoted values and missing
// VERSION_ID (returns just the ID then).
func parseOSRelease(s string) string {
	var id, ver string
	for _, line := range splitLines(s) {
		k, v := splitKV(line)
		switch k {
		case "ID":
			id = unquote(v)
		case "VERSION_ID":
			ver = unquote(v)
		}
	}
	if id == "" {
		return "linux"
	}
	if ver == "" {
		return id
	}
	return id + "-" + ver
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func splitKV(line string) (string, string) {
	for i := 0; i < len(line); i++ {
		if line[i] == '=' {
			return line[:i], line[i+1:]
		}
	}
	return line, ""
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

