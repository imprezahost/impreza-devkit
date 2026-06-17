// Package cmd holds the Cobra command tree for impreza-agent.
package cmd

import (
	"github.com/spf13/cobra"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// version is injected from main.go (via SetVersion below) which gets
// it from a build-time ldflag.
var version = "dev"

// SetVersion wires the build-time version into the root command's
// Version field and teaches the SDK to report it in the User-Agent.
// Called once from main() before Execute().
func SetVersion(v string) {
	// Identify as the agent — overrides the SDK default "impreza-sdk-go".
	sdkclient.SetUserAgent("impreza-agent")
	if v != "" {
		version = v
		rootCmd.Version = v
		sdkclient.SetVersion(v)
	}
}

// Global flags applied to every subcommand.
var (
	globalConfigPath string
)

var rootCmd = &cobra.Command{
	Use:   "impreza-agent",
	Short: "Impreza Platform managed-server daemon.",
	Long: `Impreza Platform managed-server daemon.

Runs on every managed server (Impreza-provisioned VPS / dedicated /
bring-your-own) and connects back to the control plane via HTTP
long-poll. Executes deploy / update / rollback commands and reports
outcomes.

Common workflow:

    sudo impreza-agent bootstrap --token bst_xxxxxxxxxxxxxxxx
    sudo systemctl enable --now impreza-agent

Diagnostics:

    sudo impreza-agent doctor
`,
	SilenceUsage:  true,
	SilenceErrors: false,
	Version:       version,
}

// Execute runs the root Cobra command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(
		&globalConfigPath, "config", "",
		"Override the default config path (default: /etc/impreza-agent/config.toml on Linux).",
	)

	rootCmd.AddCommand(
		bootstrapCmd,
		runCmd,
		doctorCmd,
	)
}
