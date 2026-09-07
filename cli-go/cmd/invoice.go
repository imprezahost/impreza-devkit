package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
	"github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var invoiceCmd = &cobra.Command{
	Use:   "invoice",
	Short: "Browse and pay invoices on the account.",
}

// ── invoice list ─────────────────────────────────────────────────

var invoiceListStatus string

var invoiceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the 100 most recent invoices.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		invs, err := c.InvoicesList(cmd.Context(), invoiceListStatus)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), invs, f)
		}

		if len(invs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No invoices.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "date", "due_date", "status", "total"})
		for _, inv := range invs {
			t.AppendRow(table.Row{
				inv.ID,
				inv.Date,
				inv.DueDate,
				inv.Status,
				fmt.Sprintf("%.2f %s", inv.Total, inv.Currency),
			})
		}
		t.Render()
		return nil
	},
}

// ── invoice show <id> ────────────────────────────────────────────

var invoiceShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one invoice with its line items + transactions.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invoice id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		inv, err := c.InvoiceShow(cmd.Context(), id)
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

		out := cmd.OutOrStdout()
		// Header table — invoice metadata.
		t := output.NewTable(out)
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"id", inv.ID})
		t.AppendRow(table.Row{"date", inv.Date})
		t.AppendRow(table.Row{"due_date", inv.DueDate})
		t.AppendRow(table.Row{"status", inv.Status})
		t.AppendRow(table.Row{"subtotal", fmt.Sprintf("%.2f", inv.Subtotal)})
		t.AppendRow(table.Row{"tax", fmt.Sprintf("%.2f", inv.Tax)})
		t.AppendRow(table.Row{"total", fmt.Sprintf("%.2f %s", inv.Total, inv.Currency)})
		t.Render()

		// Items sub-table.
		if len(inv.Items) > 0 {
			fmt.Fprintln(out, "\nItems:")
			ti := output.NewTable(out)
			ti.AppendHeader(table.Row{"id", "description", "type", "amount"})
			for _, it := range inv.Items {
				ti.AppendRow(table.Row{it.ID, it.Description, it.Type, fmt.Sprintf("%.2f", it.Amount)})
			}
			ti.Render()
		}

		// Transactions sub-table.
		if len(inv.Transactions) > 0 {
			fmt.Fprintln(out, "\nTransactions:")
			tt := output.NewTable(out)
			tt.AppendHeader(table.Row{"id", "date", "gateway", "amount", "transaction_id"})
			for _, tx := range inv.Transactions {
				tt.AppendRow(table.Row{tx.ID, tx.Date, tx.Gateway, fmt.Sprintf("%.2f", tx.Amount), tx.TransactionID})
			}
			tt.Render()
		}
		return nil
	},
}

// ── invoice pay <id> ─────────────────────────────────────────────

var invoicePayCmd = &cobra.Command{
	Use:   "pay <id>",
	Short: "Pay an unpaid invoice from the account credit balance.",
	Long: `Pay an unpaid invoice from the account credit balance.

This spends real money and the API offers no way to undo it, so the
invoice is read first and the prompt names the amount about to leave
the balance — confirming a number beats confirming an id. --yes skips
both the read and the prompt, for scripts.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invoice id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}

		if !autoYes {
			inv, err := c.InvoiceShow(cmd.Context(), id)
			if err != nil {
				return err
			}
			if strings.EqualFold(inv.Status, "Paid") {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Invoice %d is already paid; nothing to do.\n", id)
				return nil
			}
			if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
				fmt.Sprintf("Pay invoice %d (%.2f %s) from your account balance?",
					id, inv.Total, inv.Currency),
				autoYes); err != nil {
				return err
			}
		}

		res, err := c.InvoicePay(cmd.Context(), id)
		if err != nil {
			// 409 splits two very different outcomes: nothing to do, or
			// top up first. Say which, and don't call the first a failure.
			var cf *client.Conflict
			if errors.As(err, &cf) {
				if strings.EqualFold(cf.Code, "ALREADY_PAID") {
					fmt.Fprintf(cmd.OutOrStdout(),
						"Invoice %d is already paid; nothing to do.\n", id)
					return nil
				}
				return fmt.Errorf("%w\n  -> Add credit with: impreza account topup --amount <X> --method btc|xmr|trx|usdt", err)
			}
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), res, f)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Invoice %d paid - %.2f %s from balance.\n",
			res.InvoiceID, res.Amount, res.Currency)
		return nil
	},
}

func init() {
	invoiceListCmd.Flags().StringVar(&invoiceListStatus, "status", "",
		"Filter by status (Paid, Unpaid, Cancelled, Refunded, Collections).")
	invoicePayCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")
	invoiceCmd.AddCommand(invoiceListCmd, invoiceShowCmd, invoicePayCmd)
	rootCmd.AddCommand(invoiceCmd)
}
