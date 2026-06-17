package cmd

// `impreza login` — friendly wrapper around `impreza context create`
// that prompts interactively for the API key + secret, verifies them
// against /v1/account before persisting (so a wrong-key paste fails
// FAST + with a helpful error instead of getting a cryptic 401 on the
// next `impreza vps list` call), and sets the new context as default.
//
// Non-TTY callers (CI, scripts) can pass --key / --secret / --name as
// flags — the prompts are skipped when both --key and --secret are
// present. Verification still runs.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/imprezahost/impreza-devkit/sdk-go/client"
	sdkconfig "github.com/imprezahost/impreza-devkit/sdk-go/config"
)

var (
	loginName   string
	loginKey    string
	loginSecret string
	loginForce  bool
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Friendly wizard to set up a new API context (wraps `impreza context create`).",
	Long: `Friendly wizard for first-time setup. Prompts for your Impreza API
key + secret, verifies them against /v1/account, and stores the result
as a named local context (default name "default", set as the active one).

Generate the key + secret in your Impreza clientarea → API Management.
Whitelist this machine's public IP on the key before logging in or
the verification will fail with IP_NOT_WHITELISTED.

Non-interactive use (CI, scripts):

    impreza login --name personal --key imp_... --secret ... --force

` + "`--force`" + ` overwrites an existing context with the same name.`,
	Args: cobra.NoArgs,
	RunE: runLogin,
}

func runLogin(cmd *cobra.Command, _ []string) error {
	w := cmd.OutOrStdout()
	in := bufio.NewReader(os.Stdin)

	// Resolve name. Prompt if interactive, else default to "default".
	name := loginName
	if name == "" {
		if isStdinTTY() {
			fmt.Fprint(w, "Context name [default]: ")
			line, _ := in.ReadString('\n')
			name = strings.TrimSpace(line)
		}
		if name == "" {
			name = "default"
		}
	}

	// Resolve key.
	key := loginKey
	if key == "" {
		if !isStdinTTY() {
			return fmt.Errorf("--key is required in non-interactive mode")
		}
		fmt.Fprint(w, "API key (imp_...): ")
		line, _ := in.ReadString('\n')
		key = strings.TrimSpace(line)
	}
	if !strings.HasPrefix(key, "imp_") {
		return fmt.Errorf("API key must start with `imp_` (clientarea → API Management)")
	}

	// Resolve secret. Read with no echo when the terminal supports it.
	secret := loginSecret
	if secret == "" {
		if !isStdinTTY() {
			return fmt.Errorf("--secret is required in non-interactive mode")
		}
		fmt.Fprint(w, "API secret: ")
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(w)
		if err != nil {
			return fmt.Errorf("read secret: %w", err)
		}
		secret = strings.TrimSpace(string(raw))
	}
	if secret == "" {
		return fmt.Errorf("API secret is required")
	}

	// Check if name is already taken before we hit the network.
	existing, err := sdkconfig.Load()
	if err != nil && !errors.Is(err, sdkconfig.ErrNoConfig) {
		return err
	}
	if existing != nil {
		if _, taken := existing.Contexts[name]; taken && !loginForce {
			return fmt.Errorf("context %q already exists — pass --force to overwrite or pick a different name", name)
		}
	}

	// Verify against /v1/account so a wrong-key paste fails NOW + with
	// a helpful error (IP_NOT_WHITELISTED, AUTH_INVALID, etc.) instead
	// of silently saving + breaking the next command the user runs.
	fmt.Fprintf(w, "Verifying credentials against api.imprezahost.com ... ")
	probe, err := client.New(sdkconfig.Context{Key: key, Secret: secret})
	if err != nil {
		fmt.Fprintln(w, "failed.")
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
	defer cancel()
	info, err := probe.AccountInfo(ctx)
	if err != nil {
		fmt.Fprintln(w, "failed.")
		// Common errors get a friendlier hint added.
		hint := ""
		emsg := err.Error()
		switch {
		case strings.Contains(emsg, "IP_NOT_WHITELISTED"):
			hint = "\n  Whitelist this machine's public IP on the API key in your Impreza clientarea."
		case strings.Contains(emsg, "AUTH_INVALID"), strings.Contains(emsg, "UNAUTHORIZED"):
			hint = "\n  Double-check the key + secret were copied from the same row in clientarea → API Management."
		}
		return fmt.Errorf("%w%s", err, hint)
	}
	fmt.Fprintln(w, "ok")
	fmt.Fprintf(w, "Signed in as %s %s <%s>.\n",
		info.FirstName, info.LastName, info.Email)

	// Persist. Reuse the same config.Load + Add + Save path as
	// `impreza context create` so the on-disk shape stays canonical.
	cfg := existing
	if cfg == nil {
		cfg = sdkconfig.NewEmpty()
	}
	if loginForce {
		// `Add` rejects duplicates, but `--force` should overwrite.
		// Easiest path: delete-then-add. NoOp when name is fresh.
		_ = cfg.Delete(name)
	}
	if err := cfg.Add(name, sdkconfig.Context{Key: key, Secret: secret}); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "Context %q saved", name)
	if cfg.Default == name {
		fmt.Fprintf(w, " and set as default")
	}
	fmt.Fprintln(w, ".")
	fmt.Fprintln(w, "Try: impreza platform servers list")
	return nil
}

func isStdinTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func init() {
	loginCmd.Flags().StringVar(&loginName, "name", "",
		"Context name. Default: `default`. Use a unique name when juggling multiple accounts.")
	loginCmd.Flags().StringVar(&loginKey, "key", "",
		"API key. Required in non-interactive mode. Prompted when omitted + stdin is a TTY.")
	loginCmd.Flags().StringVar(&loginSecret, "secret", "",
		"API secret. Required in non-interactive mode. Prompted (with no echo) when omitted + stdin is a TTY.")
	loginCmd.Flags().BoolVar(&loginForce, "force", false,
		"Overwrite an existing context with the same name.")
	rootCmd.AddCommand(loginCmd)
}
