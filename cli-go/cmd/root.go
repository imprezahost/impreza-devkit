// Package cmd holds the Cobra command tree for the impreza Go CLI.
//
// Phase 7.1 ships the root command + the `context` sub-command surface
// (config-only, no network). Subsequent fases (7.2+) layer the resource
// commands on top, sharing the global flags + the HTTP client wired here.
package cmd

import (
	"github.com/spf13/cobra"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// version is injected from main.go (which gets it from a build-time ldflag).
// Defaults to "dev" so source-tree runs still report something useful.
var version = "dev"

// SetVersion is called from main() before Execute() to wire the build-time
// version string into the root command's Version field, and to teach the
// SDK to identify itself as the CLI in the User-Agent header.
func SetVersion(v string) {
	// Tell the SDK its caller — overrides the default "impreza-sdk-go".
	// Set unconditionally so dev builds also identify as the CLI.
	sdkclient.SetUserAgent("impreza-cli-go")
	if v != "" {
		version = v
		rootCmd.Version = v
		sdkclient.SetVersion(v)
	}
}

// Global flags carried on every command.
var (
	globalContext string
	globalOutput  string
	globalNoColor bool
)

var rootCmd = &cobra.Command{
	Use:   "impreza",
	Short: "Official CLI for the Impreza Host public REST API.",
	Long: `Official CLI for the Impreza Host public REST API.

Manage VPS, domains, hosting, email services, orders, invoices, and
webhook subscriptions from your shell. Pairs with the impreza-sdk
Python library for programmatic use.

Authentication: every command needs a context with your API key + secret.
Create one with: impreza context create <name> --key imp_... --secret ...
(see ` + "`impreza context --help`" + `).

Documentation:
  • SDK + REST: https://github.com/imprezahost/impreza-devkit
  • Webhooks:   openapi/asyncapi.yaml in the same repo
  • PyPI:       https://pypi.org/project/impreza-sdk and impreza-cli
`,
	SilenceUsage:  true,
	SilenceErrors: false,
	// Version is overwritten by SetVersion() before Execute() runs.
	Version: version,
}

// Execute runs the root Cobra command. Called from main(); returns the
// command's error (if any) so main can propagate the non-zero exit.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	// Global flags — apply to every command. Resource-command packages
	// (added in 7.2+) read these via the same `global*` package-level
	// vars so they don't need to thread arguments through every helper.
	rootCmd.PersistentFlags().StringVarP(
		&globalContext, "context", "c", "",
		"Override the default context for this invocation.",
	)
	rootCmd.PersistentFlags().StringVarP(
		&globalOutput, "output", "o", "table",
		"Output format: table | json | yaml.",
	)
	rootCmd.PersistentFlags().BoolVar(
		&globalNoColor, "no-color", false,
		"Disable ANSI color output (forced off on non-TTY streams already).",
	)

	// Wire sub-commands here. The `context` subcommand is the only
	// resource available in 7.1; everything else lands in 7.2+.
	rootCmd.AddCommand(contextCmd)
}
