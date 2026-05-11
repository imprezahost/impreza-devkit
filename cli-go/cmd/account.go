package cmd

import (
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var accountCmd = &cobra.Command{
	Use:   "account",
	Short: "Inspect the authenticated client profile, balance, and services.",
}

// ── account info ──────────────────────────────────────────────────

var accountInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show the authenticated client's profile + balance.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		info, err := c.AccountInfo(cmd.Context())
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), info, f)
		}

		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"id", info.ID})
		t.AppendRow(table.Row{"name", info.FirstName + " " + info.LastName})
		if info.Company != "" {
			t.AppendRow(table.Row{"company", info.Company})
		}
		t.AppendRow(table.Row{"email", info.Email})
		if info.Status != "" {
			t.AppendRow(table.Row{"status", info.Status})
		}
		t.AppendRow(table.Row{"balance", fmt.Sprintf("%.2f %s", info.Balance, info.Currency)})
		if info.RegisteredAt != "" {
			t.AppendRow(table.Row{"registered_at", info.RegisteredAt})
		}
		t.Render()
		return nil
	},
}

// ── account balance ──────────────────────────────────────────────
//
// The server doesn't expose a separate /account/balance endpoint —
// the balance comes back as part of /account (= AccountInfo). The
// CLI verb is preserved for parity with the Python CLI; it just
// re-uses AccountInfo() and prints the balance/currency fields.

var accountBalanceCmd = &cobra.Command{
	Use:   "balance",
	Short: "Print the current account balance (one line; pipe-friendly).",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		info, err := c.AccountInfo(cmd.Context())
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		// For machine-readable formats, emit just the balance pair so
		// scripts don't have to parse the whole AccountInfo blob.
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), map[string]any{
				"balance":  info.Balance,
				"currency": info.Currency,
			}, f)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%.2f %s\n", info.Balance, info.Currency)
		return nil
	},
}

// ── account services ─────────────────────────────────────────────

var accountServicesStatus string

var accountServicesCmd = &cobra.Command{
	Use:   "services",
	Short: "List every service (VPS, domain, hosting, email) on the account.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		svcs, err := c.AccountServices(cmd.Context(), accountServicesStatus)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), svcs, f)
		}

		if len(svcs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No services on this account.")
			return nil
		}

		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "product", "group", "status", "domain", "amount", "next due"})
		for _, s := range svcs {
			amount := ""
			if s.Amount > 0 {
				amount = fmt.Sprintf("%.2f", s.Amount)
			}
			t.AppendRow(table.Row{
				s.ID,
				s.Product,
				s.ProductGroup,
				s.Status,
				s.Domain,
				amount,
				s.NextDue,
			})
		}
		t.Render()
		return nil
	},
}

func init() {
	accountServicesCmd.Flags().StringVar(&accountServicesStatus, "status", "",
		"Filter by status (Active, Suspended, Terminated, Pending, Cancelled).")

	accountCmd.AddCommand(accountInfoCmd, accountBalanceCmd, accountServicesCmd)
	rootCmd.AddCommand(accountCmd)
}
