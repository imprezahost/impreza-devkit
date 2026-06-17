package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// DNS write verbs hang off the `domain dns` sub-command tree already
// defined in cmd/domain.go (which mounts `domain dns list` from
// Phase 7.2). 7.3 adds add / update / delete / activate.
//
// All three mutation verbs identify records by content tuple
// (type, host, value/old_value), matching the server's wire contract
// — there are no stable URL ids for DNS records on the API.

// ── domain dns add ───────────────────────────────────────────────

var (
	dnsAddType     string
	dnsAddHost     string
	dnsAddValue    string
	dnsAddTTL      int
	dnsAddPriority int
)

var domainDnsAddCmd = &cobra.Command{
	Use:   "add <domain>",
	Short: "Create a new DNS record on a domain.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if dnsAddType == "" || dnsAddHost == "" || dnsAddValue == "" {
			return errors.New("--type, --host, and --value are required")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		req := client.DnsAddRequest{
			Type: dnsAddType, Host: dnsAddHost, Value: dnsAddValue,
			TTL: dnsAddTTL, Priority: dnsAddPriority,
		}
		if err := c.DomainDnsAdd(cmd.Context(), args[0], req); err != nil {
			return err
		}
		output.Success("%s record %s on %s created (value=%s).",
			req.Type, req.Host, args[0], req.Value)
		return nil
	},
}

// ── domain dns update ────────────────────────────────────────────

var (
	dnsUpdateType     string
	dnsUpdateHost     string
	dnsUpdateOldValue string
	dnsUpdateNewValue string
	dnsUpdateTTL      int
	dnsUpdatePriority int
)

var domainDnsUpdateCmd = &cobra.Command{
	Use:   "update <domain>",
	Short: "Edit an existing DNS record (matched by type + host + old-value).",
	Long: `Update a DNS record. Records are matched by the tuple
(type, host, old-value) — pass all three. --new-value replaces the
value; --ttl / --priority replace the metadata if non-zero.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if dnsUpdateType == "" || dnsUpdateHost == "" ||
			dnsUpdateOldValue == "" || dnsUpdateNewValue == "" {
			return errors.New("--type, --host, --old-value, and --new-value are required")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		req := client.DnsUpdateRequest{
			Type:     dnsUpdateType,
			Host:     dnsUpdateHost,
			OldValue: dnsUpdateOldValue,
			NewValue: dnsUpdateNewValue,
			TTL:      dnsUpdateTTL,
			Priority: dnsUpdatePriority,
		}
		if err := c.DomainDnsUpdate(cmd.Context(), args[0], req); err != nil {
			return err
		}
		output.Success("%s record %s on %s updated: %s → %s.",
			req.Type, req.Host, args[0], req.OldValue, req.NewValue)
		return nil
	},
}

// ── domain dns delete ────────────────────────────────────────────

var (
	dnsDeleteType  string
	dnsDeleteHost  string
	dnsDeleteValue string
)

var domainDnsDeleteCmd = &cobra.Command{
	Use:     "delete <domain>",
	Aliases: []string{"rm", "remove"},
	Short:   "Delete a DNS record (matched by type + host + value).",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if dnsDeleteType == "" || dnsDeleteHost == "" || dnsDeleteValue == "" {
			return errors.New("--type, --host, and --value are required")
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Delete %s record %s on %s (value=%s)?",
				dnsDeleteType, dnsDeleteHost, args[0], dnsDeleteValue),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DomainDnsDelete(cmd.Context(), args[0], client.DnsDeleteRequest{
			Type: dnsDeleteType, Host: dnsDeleteHost, Value: dnsDeleteValue,
		}); err != nil {
			return err
		}
		output.Success("%s record %s on %s deleted.", dnsDeleteType, dnsDeleteHost, args[0])
		return nil
	},
}

// ── domain dns activate ──────────────────────────────────────────

var domainDnsActivateCmd = &cobra.Command{
	Use:   "activate <domain>",
	Short: "Enable Impreza-hosted DNS for a domain (required before first add).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DomainDnsActivate(cmd.Context(), args[0]); err != nil {
			return err
		}
		output.Success("DNS activated for %s. Add records with `impreza domain dns add %s --type A ...`.",
			args[0], args[0])
		return nil
	},
}

func init() {
	// `add` flags
	domainDnsAddCmd.Flags().StringVar(&dnsAddType, "type", "",
		"Record type: A | AAAA | CNAME | MX | TXT | NS | SRV.")
	domainDnsAddCmd.Flags().StringVar(&dnsAddHost, "host", "",
		"Record host name (e.g. 'www', or '@' for the apex).")
	domainDnsAddCmd.Flags().StringVar(&dnsAddHost, "name", "",
		"Alias for --host.")
	domainDnsAddCmd.Flags().StringVar(&dnsAddValue, "value", "",
		"Record value (IP, hostname, text content, etc.).")
	domainDnsAddCmd.Flags().IntVar(&dnsAddTTL, "ttl", 0,
		"TTL in seconds (server requires ≥ 7200; omit for zone default).")
	domainDnsAddCmd.Flags().IntVar(&dnsAddPriority, "priority", 0,
		"Priority — required for MX / SRV records.")

	// `update` flags — same shape as `add` but with old/new value pair
	domainDnsUpdateCmd.Flags().StringVar(&dnsUpdateType, "type", "",
		"Record type to update (must match the existing record).")
	domainDnsUpdateCmd.Flags().StringVar(&dnsUpdateHost, "host", "",
		"Record host (must match the existing record).")
	domainDnsUpdateCmd.Flags().StringVar(&dnsUpdateOldValue, "old-value", "",
		"Existing value the record currently carries.")
	domainDnsUpdateCmd.Flags().StringVar(&dnsUpdateNewValue, "new-value", "",
		"New value to set.")
	domainDnsUpdateCmd.Flags().IntVar(&dnsUpdateTTL, "ttl", 0,
		"New TTL (server requires ≥ 7200 if changing).")
	domainDnsUpdateCmd.Flags().IntVar(&dnsUpdatePriority, "priority", 0,
		"New priority (MX / SRV).")

	// `delete` flags
	domainDnsDeleteCmd.Flags().StringVar(&dnsDeleteType, "type", "",
		"Record type to delete.")
	domainDnsDeleteCmd.Flags().StringVar(&dnsDeleteHost, "host", "",
		"Record host name.")
	domainDnsDeleteCmd.Flags().StringVar(&dnsDeleteValue, "value", "",
		"Record value the entry currently carries.")

	domainDnsCmd.AddCommand(domainDnsAddCmd, domainDnsUpdateCmd, domainDnsDeleteCmd, domainDnsActivateCmd)
}
