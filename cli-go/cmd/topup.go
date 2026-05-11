package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// Top-up verbs live on the `account` sub-app (mounted in cmd/account.go).
// 7.5 adds `account topup` and `account topup-status`.

var (
	topupAmount  float64
	topupMethod  string
	topupBrowser bool
	topupWait    bool
	topupTimeout int
)

var accountTopupCmd = &cobra.Command{
	Use:   "topup",
	Short: "Create a crypto top-up invoice. Optionally open in browser + poll until paid.",
	Long: `Generates a new top-up invoice for the supplied --amount.

By default the invoice is returned with a payment_url pointing at the
btcpayinline gateway — copy/paste into a browser. Pass --browser to
auto-open the URL in your default browser. Pass --wait to poll the
invoice until it reaches a terminal state (paid / expired / cancelled).

The wait poll renders elapsed seconds + ETA-until-expiry on a single
in-place redrawn line. --timeout caps the total wait (default 7200 s
= 2 h, matching the server-side invoice TTL).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if topupAmount <= 0 {
			return errors.New("--amount is required (e.g. --amount 50)")
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		inv, err := c.AccountTopup(cmd.Context(), client.AccountTopupRequest{
			Amount: topupAmount,
			Method: topupMethod,
		})
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}

		// Render the invoice immediately so the user sees the
		// payment URL even when --wait is set.
		if f == output.FormatTable {
			renderTopupInvoice(cmd.OutOrStdout(), inv)
		}

		// Auto-open if requested (suppressed for non-table output —
		// JSON/YAML consumers are scripts, opening a browser would
		// be a side-effect surprise).
		if topupBrowser && f == output.FormatTable {
			openBrowser(cmd.ErrOrStderr(), inv.PaymentURL)
		}

		if !topupWait {
			// Machine-readable output: render the invoice now.
			if f != output.FormatTable {
				return renderJSONOrYAML(cmd.OutOrStdout(), inv, f)
			}
			return nil
		}

		// Poll loop.
		timeout := time.Duration(topupTimeout) * time.Second
		if topupTimeout == 0 {
			timeout = 2 * time.Hour
		}
		start := time.Now()
		const padding = 100
		err = inv.WaitUntilPaid(cmd.Context(), client.TopupWaitOptions{
			Timeout:      timeout,
			PollInterval: 5 * time.Second,
			OnUpdate: func(inv *client.TopupInvoice) {
				elapsed := time.Since(start).Round(time.Second)
				eta := ""
				if d, ok := parseExpiryDelta(inv.ExpiresAt); ok && d > 0 {
					eta = fmt.Sprintf(" / %s until expiry", d.Round(time.Second))
				}
				line := fmt.Sprintf("Waiting on top-up invoice %d — %s elapsed%s (status=%s)",
					inv.InvoiceID, elapsed, eta, inv.Status)
				fmt.Fprintf(cmd.ErrOrStderr(), "\r%-*s", padding, line)
			},
		})
		fmt.Fprintln(cmd.ErrOrStderr())

		if err != nil {
			if errors.Is(err, client.ErrTopupExpired) {
				output.Warning("Top-up invoice %d settled in non-paid state: %s", inv.InvoiceID, inv.Status)
			}
			return err
		}
		output.Success("Top-up invoice %d paid.", inv.InvoiceID)
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), inv, f)
		}
		// Re-render the settled invoice in-table.
		renderTopupInvoice(cmd.OutOrStdout(), inv)
		return nil
	},
}

var accountTopupStatusCmd = &cobra.Command{
	Use:   "topup-status <invoice-id>",
	Short: "Show the latest status of an existing top-up invoice.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invoice-id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		inv, err := c.AccountTopupStatus(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), inv, f)
		}
		renderTopupInvoice(cmd.OutOrStdout(), inv)
		return nil
	},
}

func renderTopupInvoice(w io.Writer, inv *client.TopupInvoice) {
	t := output.NewTable(w)
	t.AppendHeader(table.Row{"field", "value"})
	t.AppendRow(table.Row{"invoice_id", inv.InvoiceID})
	t.AppendRow(table.Row{"amount", fmt.Sprintf("%.2f %s", inv.Amount, inv.Currency)})
	t.AppendRow(table.Row{"method", inv.Method})
	t.AppendRow(table.Row{"status", inv.Status})
	if inv.PaymentURL != "" {
		t.AppendRow(table.Row{"payment_url", inv.PaymentURL})
	}
	if inv.CryptoAddress != "" {
		t.AppendRow(table.Row{"crypto_address", inv.CryptoAddress})
	}
	if inv.ExpiresAt != "" {
		t.AppendRow(table.Row{"expires_at", inv.ExpiresAt})
	}
	if inv.CreatedAt != "" {
		t.AppendRow(table.Row{"created_at", inv.CreatedAt})
	}
	if inv.BalanceAfter != nil {
		t.AppendRow(table.Row{"balance_after", fmt.Sprintf("%.2f", *inv.BalanceAfter)})
	}
	t.Render()
}

// parseExpiryDelta parses a server-emitted ISO-8601 timestamp and
// returns the duration from now to then. Returns ok=false if the
// string can't be parsed.
func parseExpiryDelta(iso string) (time.Duration, bool) {
	if iso == "" {
		return 0, false
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(layout, iso); err == nil {
			return time.Until(t), true
		}
	}
	return 0, false
}

// openBrowser invokes the OS-specific "open <url>" command. Best-
// effort — prints a friendly fallback to stderr on failure.
func openBrowser(errOut io.Writer, url string) {
	if url == "" {
		fmt.Fprintln(errOut, "No payment URL on the invoice (gateway may be already-settled).")
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		fmt.Fprintf(errOut, "(no browser handler for %s; payment URL: %s)\n", runtime.GOOS, url)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(errOut, "(couldn't auto-open browser: %v; payment URL: %s)\n", err, url)
		return
	}
	// Detach: we don't care about exit code.
	go func() { _ = cmd.Wait() }()
}

func init() {
	accountTopupCmd.Flags().Float64Var(&topupAmount, "amount", 0,
		"Top-up amount in the account currency (USD). Must be ≥ server minimum (typically $1).")
	accountTopupCmd.Flags().StringVar(&topupMethod, "method", "",
		"Payment method: btc | xmr | trx | usdt | usdt_trc20. Gateway may show alternatives if the preferred isn't available.")
	accountTopupCmd.Flags().BoolVar(&topupBrowser, "browser", false,
		"Auto-open the payment_url in the system default browser.")
	accountTopupCmd.Flags().BoolVar(&topupWait, "wait", false,
		"Block until the invoice settles (paid / expired / cancelled).")
	accountTopupCmd.Flags().IntVar(&topupTimeout, "timeout", 7200,
		"Wait timeout in seconds (default 7200 = 2 h, matches server-side TTL).")

	accountCmd.AddCommand(accountTopupCmd, accountTopupStatusCmd)
}

// _ keeps the import for context until used (e.g. tests or future
// streaming variants).
var _ = context.Background
