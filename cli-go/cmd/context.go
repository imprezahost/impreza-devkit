package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/config"
)

var contextCmd = &cobra.Command{
	Use:   "context",
	Short: "Manage local API contexts (named credential profiles).",
	Long: `Contexts are local-only — they live in your config file and never
hit the network. Each context bundles an API key + secret + optional
URL / Tor / proxy overrides under a human-readable name.

Typical first run:

    impreza context create personal --key imp_... --secret ...

Then everyday commands ` + "`impreza vps list`" + ` read the default context
automatically. Switch with ` + "`impreza context use other`" + ` or override
per-invocation with the global ` + "`--context NAME`" + ` flag.`,
}

// ── context create ─────────────────────────────────────────────────

var (
	createKey    string
	createSecret string
	createURL    string
	createUseTor bool
	createProxy  string
)

var contextCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Store a new named API context locally.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if createKey == "" || createSecret == "" {
			return fmt.Errorf("both --key and --secret are required (generate them in your Impreza Account → API Management)")
		}

		cfg, err := config.Load()
		if errors.Is(err, config.ErrNoConfig) {
			cfg = config.NewEmpty()
		} else if err != nil {
			return err
		}

		entry := config.Context{
			Key:    createKey,
			Secret: createSecret,
			URL:    createURL,
			UseTor: createUseTor,
			Proxy:  createProxy,
		}
		if err := cfg.Add(name, entry); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Context %q created", name)
		if cfg.Default == name {
			fmt.Fprintf(out, " and set as default")
		}
		fmt.Fprintln(out, ".")
		return nil
	},
}

func init() {
	contextCreateCmd.Flags().StringVar(&createKey, "key", "",
		"API key (starts with imp_). Required.")
	contextCreateCmd.Flags().StringVar(&createSecret, "secret", "",
		"API secret. Required. Shown once on key creation; rotate if it leaks.")
	contextCreateCmd.Flags().StringVar(&createURL, "url", "",
		"Override the default https://api.imprezahost.com base URL.")
	contextCreateCmd.Flags().BoolVar(&createUseTor, "use-tor", false,
		"Force-route this context's requests through a local SOCKS5 Tor proxy.")
	contextCreateCmd.Flags().StringVar(&createProxy, "proxy", "",
		"Custom SOCKS5 proxy URL (e.g. socks5://127.0.0.1:9050). Implies --use-tor semantics.")

	contextCmd.AddCommand(contextCreateCmd)
}

// ── context use ────────────────────────────────────────────────────

var contextUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the default context for future invocations.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err := cfg.SetDefault(args[0]); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Default context now %q.\n", args[0])
		return nil
	},
}

func init() {
	contextCmd.AddCommand(contextUseCmd)
}

// ── context list ───────────────────────────────────────────────────

var contextListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all stored contexts.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if errors.Is(err, config.ErrNoConfig) {
			fmt.Fprintln(cmd.OutOrStdout(), "No contexts yet. Create one with: impreza context create <name> --key ... --secret ...")
			return nil
		}
		if err != nil {
			return err
		}
		if len(cfg.Contexts) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No contexts yet. Create one with: impreza context create <name> --key ... --secret ...")
			return nil
		}

		names := make([]string, 0, len(cfg.Contexts))
		for n := range cfg.Contexts {
			names = append(names, n)
		}
		sort.Strings(names)

		out := cmd.OutOrStdout()
		for _, n := range names {
			c := cfg.Contexts[n]
			marker := "  "
			if n == cfg.Default {
				marker = "* "
			}
			fmt.Fprintf(out, "%s%-20s  %s\n", marker, n, maskKey(c.Key))
		}
		return nil
	},
}

// maskKey returns "imp_a1b2…" — the first 8 chars then an ellipsis.
// Mirrors what `impreza doctor` displays in the Python CLI.
func maskKey(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8] + "…"
}

func init() {
	contextCmd.AddCommand(contextListCmd)
}

// ── context current ────────────────────────────────────────────────

var contextCurrentCmd = &cobra.Command{
	Use:   "current",
	Short: "Print the name of the currently active context.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		name, _, err := cfg.Active(globalContext)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), name)
		return nil
	},
}

func init() {
	contextCmd.AddCommand(contextCurrentCmd)
}

// ── context delete ─────────────────────────────────────────────────

var contextDeleteCmd = &cobra.Command{
	Use:     "delete <name>",
	Aliases: []string{"rm", "remove"},
	Short:   "Remove a stored context.",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		wasDefault := cfg.Default == name
		if err := cfg.Delete(name); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Context %q deleted.\n", name)
		if wasDefault {
			remaining := make([]string, 0, len(cfg.Contexts))
			for n := range cfg.Contexts {
				remaining = append(remaining, n)
			}
			sort.Strings(remaining)
			if len(remaining) == 0 {
				fmt.Fprintln(out, "No default context now — create one with `impreza context create`.")
			} else {
				fmt.Fprintf(out, "No default context now — set one with `impreza context use {%s}`.\n",
					strings.Join(remaining, "|"))
			}
		}
		return nil
	},
}

func init() {
	contextCmd.AddCommand(contextDeleteCmd)
}
