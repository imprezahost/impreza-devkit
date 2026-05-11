package cmd

import (
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "Inspect the calling API key's identity + whitelist.",
}

var keyWhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show the calling key's prefix, label, status, IP whitelist, and what IP the server saw.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		id, err := c.ApiKeySelf(cmd.Context())
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), id, f)
		}

		out := cmd.OutOrStdout()
		t := output.NewTable(out)
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"id", id.ID})
		t.AppendRow(table.Row{"prefix", id.Prefix})
		if id.Label != "" {
			t.AppendRow(table.Row{"label", id.Label})
		}
		t.AppendRow(table.Row{"status", id.Status})
		if id.RateLimitPerMin > 0 {
			t.AppendRow(table.Row{"rate_limit_per_minute", id.RateLimitPerMin})
		}
		if id.RequestIP != "" {
			t.AppendRow(table.Row{"request_ip", id.RequestIP})
		}
		if id.LastUsedAt != "" {
			t.AppendRow(table.Row{"last_used_at", id.LastUsedAt})
		}
		if id.CreatedAt != "" {
			t.AppendRow(table.Row{"created_at", id.CreatedAt})
		}
		t.Render()

		// IP whitelist sub-table.
		if len(id.IPWhitelist) > 0 {
			fmt.Fprintln(out, "\nIP whitelist:")
			tw := output.NewTable(out)
			tw.AppendHeader(table.Row{"id", "ip_address", "label", "created_at"})
			for _, e := range id.IPWhitelist {
				tw.AppendRow(table.Row{e.ID, e.IPAddress, e.Label, e.CreatedAt})
			}
			tw.Render()

			// Surface the "is request_ip in the whitelist?" answer so
			// the user doesn't have to grep.
			if id.RequestIP != "" {
				match := ""
				for _, e := range id.IPWhitelist {
					if e.IPAddress == id.RequestIP {
						match = e.Label
						if match == "" {
							match = "(no label)"
						}
						break
					}
				}
				if match != "" {
					fmt.Fprintf(out, "\nrequest_ip %s matches whitelist entry: %s\n", id.RequestIP, match)
				} else {
					labels := make([]string, 0, len(id.IPWhitelist))
					for _, e := range id.IPWhitelist {
						labels = append(labels, e.IPAddress)
					}
					fmt.Fprintf(out, "\nrequest_ip %s NOT in whitelist (%s)\n",
						id.RequestIP, strings.Join(labels, ", "))
				}
			}
		}
		return nil
	},
}

func init() {
	keyCmd.AddCommand(keyWhoamiCmd)
	rootCmd.AddCommand(keyCmd)
}
