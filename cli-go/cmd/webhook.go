package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var webhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Webhook subscription management + delivery history.",
	Long: `Webhook subscriptions push events to your URL whenever something
happens on your account (top-up paid, VPS power state change, domain
expiring soon, etc.).

Each subscription has a secret returned ONCE on create — store it
securely and verify incoming requests with HMAC-SHA256.

See the event-payload contract in openapi/asyncapi.yaml.`,
}

// ── webhook list ─────────────────────────────────────────────────

var webhookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List webhook subscriptions on the account.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		subs, err := c.WebhooksList(cmd.Context())
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), subs, f)
		}
		if len(subs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No webhook subscriptions.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "url", "events", "is_active", "last_delivery_at", "last_status"})
		for _, s := range subs {
			t.AppendRow(table.Row{
				s.ID, s.URL,
				strings.Join(s.Events, ","),
				s.IsActive, s.LastDeliveryAt,
				s.LastDeliveryStatus,
			})
		}
		t.Render()
		return nil
	},
}

// ── webhook show ─────────────────────────────────────────────────

var webhookShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one webhook subscription.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		s, err := c.WebhookShow(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), s, f)
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"id", s.ID})
		t.AppendRow(table.Row{"url", s.URL})
		t.AppendRow(table.Row{"events", strings.Join(s.Events, ", ")})
		if s.Description != "" {
			t.AppendRow(table.Row{"description", s.Description})
		}
		t.AppendRow(table.Row{"is_active", s.IsActive})
		t.AppendRow(table.Row{"last_delivery_at", s.LastDeliveryAt})
		t.AppendRow(table.Row{"last_delivery_status", s.LastDeliveryStatus})
		t.AppendRow(table.Row{"created_at", s.CreatedAt})
		t.Render()
		return nil
	},
}

// ── webhook create ───────────────────────────────────────────────

var (
	webhookCreateURL         string
	webhookCreateEvents      string
	webhookCreateDescription string
)

var webhookCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Register a new webhook subscription. Secret is shown ONCE — store it.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if webhookCreateURL == "" || webhookCreateEvents == "" {
			return errors.New("--url and --events are required")
		}
		events := []string{}
		for _, e := range strings.Split(webhookCreateEvents, ",") {
			if e = strings.TrimSpace(e); e != "" {
				events = append(events, e)
			}
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		s, err := c.WebhookCreate(cmd.Context(), client.WebhookCreateRequest{
			URL: webhookCreateURL, Events: events, Description: webhookCreateDescription,
		})
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), s, f)
		}
		output.Success("Webhook subscription %d created.", s.ID)
		output.Warning("Secret (shown only once — store it now): %s", s.Secret)
		if s.SecretWarning != "" {
			output.Info("%s", s.SecretWarning)
		}
		return nil
	},
}

// ── webhook update ───────────────────────────────────────────────

var (
	webhookUpdateURL         string
	webhookUpdateEvents      string
	webhookUpdateDescription string
	webhookUpdateActive      string // "true" / "false" / "" (unset)
)

var webhookUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update fields of an existing subscription (any omitted flag is left unchanged).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("id must be an integer: %s", args[0])
		}
		req := client.WebhookUpdateRequest{
			URL:         webhookUpdateURL,
			Description: webhookUpdateDescription,
		}
		if webhookUpdateEvents != "" {
			for _, e := range strings.Split(webhookUpdateEvents, ",") {
				if e = strings.TrimSpace(e); e != "" {
					req.Events = append(req.Events, e)
				}
			}
		}
		if webhookUpdateActive != "" {
			active, err := strconv.ParseBool(webhookUpdateActive)
			if err != nil {
				return fmt.Errorf("--active must be true|false: %s", webhookUpdateActive)
			}
			req.IsActive = &active
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		s, err := c.WebhookUpdate(cmd.Context(), id, req)
		if err != nil {
			return err
		}
		output.Success("Webhook subscription %d updated.", s.ID)
		return nil
	},
}

// ── webhook delete ───────────────────────────────────────────────

var webhookDeleteCmd = &cobra.Command{
	Use:     "delete <id>",
	Aliases: []string{"rm", "remove"},
	Short:   "Remove a webhook subscription.",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Delete webhook subscription %d?", id), autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.WebhookDelete(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("Webhook subscription %d deleted.", id)
		return nil
	},
}

// ── webhook rotate-secret ────────────────────────────────────────

var webhookRotateSecretCmd = &cobra.Command{
	Use:   "rotate-secret <id>",
	Short: "Rotate the HMAC secret of a subscription. New secret shown ONCE.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		s, err := c.WebhookRotateSecret(cmd.Context(), id)
		if err != nil {
			return err
		}
		output.Success("Webhook subscription %d secret rotated.", s.ID)
		output.Warning("New secret (shown only once — store it now): %s", s.Secret)
		return nil
	},
}

// ── webhook deliveries ───────────────────────────────────────────

var webhookDeliveriesCmd = &cobra.Command{
	Use:   "deliveries <id>",
	Short: "Show recent delivery attempts for a subscription.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		ds, err := c.WebhookDeliveries(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), ds, f)
		}
		if len(ds) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No deliveries yet.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "event_type", "event_id", "attempts", "delivered", "status_code", "last_at"})
		for _, d := range ds {
			t.AppendRow(table.Row{
				d.ID, d.EventType, d.EventID, d.Attempts, d.Delivered,
				d.LastResponseCode, d.LastAttemptedAt,
			})
		}
		t.Render()
		return nil
	},
}

// ── webhook event-types ──────────────────────────────────────────

var webhookEventTypesCmd = &cobra.Command{
	Use:   "event-types",
	Short: "List the events you can subscribe to (concrete types + wildcards).",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		cat, err := c.WebhookEventTypes(cmd.Context())
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), cat, f)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Event types:")
		for _, e := range cat.EventTypes {
			fmt.Fprintf(out, "  %s\n", e)
		}
		fmt.Fprintln(out, "\nWildcards:")
		for pat, desc := range cat.Wildcards {
			fmt.Fprintf(out, "  %-10s  %s\n", pat, desc)
		}
		return nil
	},
}

func init() {
	webhookCreateCmd.Flags().StringVar(&webhookCreateURL, "url", "",
		"Subscriber URL. Must accept POST + return 2xx within ~10s to count as delivered.")
	webhookCreateCmd.Flags().StringVar(&webhookCreateEvents, "events", "",
		"Comma-separated event types or wildcards (e.g. 'topup.paid,vps.*'). Run `impreza webhook event-types` for the catalog.")
	webhookCreateCmd.Flags().StringVar(&webhookCreateDescription, "description", "",
		"Free-text description (shown in `webhook list`).")

	webhookUpdateCmd.Flags().StringVar(&webhookUpdateURL, "url", "",
		"New subscriber URL.")
	webhookUpdateCmd.Flags().StringVar(&webhookUpdateEvents, "events", "",
		"New comma-separated event subscription set (replaces, doesn't append).")
	webhookUpdateCmd.Flags().StringVar(&webhookUpdateDescription, "description", "",
		"New description.")
	webhookUpdateCmd.Flags().StringVar(&webhookUpdateActive, "active", "",
		"Set active state: 'true' | 'false'. Leave empty to keep current.")

	webhookDeleteCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	webhookCmd.AddCommand(
		webhookListCmd, webhookShowCmd, webhookCreateCmd, webhookUpdateCmd,
		webhookDeleteCmd, webhookRotateSecretCmd, webhookDeliveriesCmd,
		webhookEventTypesCmd,
	)
	rootCmd.AddCommand(webhookCmd)
}
