package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var orderCmd = &cobra.Command{
	Use:   "order",
	Short: "Browse + place product orders.",
}

// ── order list ───────────────────────────────────────────────────

var orderListStatus string

var orderListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent orders.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		os, err := c.OrdersList(cmd.Context(), orderListStatus)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), os, f)
		}
		if len(os) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No orders.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "order_number", "date", "status", "total"})
		for _, o := range os {
			t.AppendRow(table.Row{
				o.ID, o.OrderNumber, o.Date, o.Status,
				fmt.Sprintf("%.2f %s", o.Amount, o.Currency),
			})
		}
		t.Render()
		return nil
	},
}

// ── order show ───────────────────────────────────────────────────

var orderShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one order with its line items.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("order id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		o, err := c.OrderShow(cmd.Context(), id)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), o, f)
		}

		out := cmd.OutOrStdout()
		t := output.NewTable(out)
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"id", o.ID})
		t.AppendRow(table.Row{"order_number", o.OrderNumber})
		t.AppendRow(table.Row{"date", o.Date})
		t.AppendRow(table.Row{"status", o.Status})
		t.AppendRow(table.Row{"total", fmt.Sprintf("%.2f %s", o.Amount, o.Currency)})
		if o.InvoiceID > 0 {
			t.AppendRow(table.Row{"invoice_id", o.InvoiceID})
		}
		t.Render()

		if len(o.Items) > 0 {
			fmt.Fprintln(out, "\nItems:")
			ti := output.NewTable(out)
			ti.AppendHeader(table.Row{"service_id", "product", "domain", "billing_cycle", "status", "amount"})
			for _, it := range o.Items {
				ti.AppendRow(table.Row{it.ServiceID, it.Product, it.Domain, it.BillingCycle, it.Status, it.Amount})
			}
			ti.Render()
		}
		return nil
	},
}

// ── order create ─────────────────────────────────────────────────

var (
	orderProductID     int
	orderBillingCycle  string
	orderDomain        string
	orderHostname      string
	orderConfigOpts    []string
	orderCustomFields  []string
	orderPaymentMethod string
)

var orderCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Place a new product order (charges account balance unless --payment-method overrides).",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if orderProductID == 0 || orderBillingCycle == "" {
			return errors.New("--product-id and --billing-cycle are required")
		}

		configOpts, err := parseIntPairs(orderConfigOpts, "--config-option")
		if err != nil {
			return err
		}
		customFields, err := parseStringPairs(orderCustomFields, "--custom-field")
		if err != nil {
			return err
		}

		req := client.OrderCreateRequest{
			ProductID:     orderProductID,
			BillingCycle:  orderBillingCycle,
			Domain:        orderDomain,
			Hostname:      orderHostname,
			ConfigOptions: configOpts,
			CustomFields:  customFields,
			PaymentMethod: orderPaymentMethod,
		}

		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Place order for product %d (cycle=%s)? The cost will be charged from your account balance.",
				orderProductID, orderBillingCycle),
			autoYes); err != nil {
			return err
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		res, err := c.OrderCreate(cmd.Context(), req)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), res, f)
		}

		output.Success("Order %d created (order_number=%d).", res.OrderID, res.OrderNumber)
		if res.InvoiceID > 0 {
			output.Info("Invoice %d generated for %.2f %s.", res.InvoiceID, res.Amount, res.Currency)
		}
		if res.BalanceAfter != nil {
			output.Info("Account balance now: %.2f %s.", *res.BalanceAfter, res.Currency)
		}
		return nil
	},
}

// ── order upgrade ────────────────────────────────────────────────

var (
	upgradeServiceID    int
	upgradeProductID    int
	upgradeBillingCycle string
	upgradeConfigOpts   []string
	upgradePayment      string
)

var orderUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade an existing service to a different product / cycle.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if upgradeServiceID == 0 || upgradeProductID == 0 || upgradeBillingCycle == "" {
			return errors.New("--service-id, --product-id, and --billing-cycle are required")
		}
		configOpts, err := parseIntPairs(upgradeConfigOpts, "--config-option")
		if err != nil {
			return err
		}

		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Upgrade service %d to product %d (cycle=%s)? Pro-rata cost is charged from balance.",
				upgradeServiceID, upgradeProductID, upgradeBillingCycle),
			autoYes); err != nil {
			return err
		}

		c, _, err := newClient()
		if err != nil {
			return err
		}
		res, err := c.OrderUpgrade(cmd.Context(), upgradeServiceID, client.OrderUpgradeRequest{
			ProductID:     upgradeProductID,
			BillingCycle:  upgradeBillingCycle,
			ConfigOptions: configOpts,
			PaymentMethod: upgradePayment,
		})
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), res, f)
		}
		output.Success("Upgrade order %d created (order_number=%d).", res.OrderID, res.OrderNumber)
		if res.InvoiceID > 0 {
			output.Info("Invoice %d generated for %.2f %s.", res.InvoiceID, res.Amount, res.Currency)
		}
		return nil
	},
}

// parseIntPairs converts ["1=2", "3=4"] to {1:2, 3:4}. Used for
// --config-option (server expects int:int dicts on the wire).
func parseIntPairs(input []string, flag string) (map[string]int, error) {
	if len(input) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(input))
	for _, kv := range input {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("%s expects key=value pairs (got %q)", flag, kv)
		}
		_, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("%s key must be an integer id (got %q)", flag, k)
		}
		vInt, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s value must be an integer id (got %q)", flag, v)
		}
		out[k] = vInt
	}
	return out, nil
}

// parseStringPairs converts ["1=foo", "2=bar"] to {1:"foo", 2:"bar"}.
// Used for --custom-field (server wants int-keyed string values).
func parseStringPairs(input []string, flag string) (map[string]string, error) {
	if len(input) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(input))
	for _, kv := range input {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("%s expects key=value pairs (got %q)", flag, kv)
		}
		out[k] = v
	}
	return out, nil
}

func init() {
	orderListCmd.Flags().StringVar(&orderListStatus, "status", "",
		"Filter by status (Pending, Active, Fraud, Cancelled).")

	orderCreateCmd.Flags().IntVar(&orderProductID, "product-id", 0,
		"Product id from `impreza catalog products`.")
	orderCreateCmd.Flags().StringVar(&orderBillingCycle, "billing-cycle", "",
		"Billing cycle: monthly | quarterly | semiannually | annually | biennially | triennially.")
	orderCreateCmd.Flags().StringVar(&orderDomain, "domain", "",
		"Required for hosting/domain products; optional for VPS.")
	orderCreateCmd.Flags().StringVar(&orderHostname, "hostname", "",
		"VPS hostname (optional).")
	orderCreateCmd.Flags().StringArrayVar(&orderConfigOpts, "config-option", nil,
		"Config option as 'id=sub_id'. Repeatable. See `impreza catalog product <id>` for the option ids.")
	orderCreateCmd.Flags().StringArrayVar(&orderCustomFields, "custom-field", nil,
		"Custom field as 'id=value'. Repeatable.")
	orderCreateCmd.Flags().StringVar(&orderPaymentMethod, "payment-method", "",
		"Payment gateway slug; 'credit' uses account balance (default).")
	orderCreateCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	orderUpgradeCmd.Flags().IntVar(&upgradeServiceID, "service-id", 0,
		"Service id to upgrade.")
	orderUpgradeCmd.Flags().IntVar(&upgradeProductID, "product-id", 0,
		"Target product id.")
	orderUpgradeCmd.Flags().StringVar(&upgradeBillingCycle, "billing-cycle", "",
		"Target billing cycle.")
	orderUpgradeCmd.Flags().StringArrayVar(&upgradeConfigOpts, "config-option", nil,
		"Config option as 'id=sub_id'. Repeatable.")
	orderUpgradeCmd.Flags().StringVar(&upgradePayment, "payment-method", "",
		"Payment gateway slug; 'credit' uses balance (default).")
	orderUpgradeCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	orderCmd.AddCommand(orderListCmd, orderShowCmd, orderCreateCmd, orderUpgradeCmd)
	rootCmd.AddCommand(orderCmd)
}
