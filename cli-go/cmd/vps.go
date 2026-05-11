package cmd

import (
	"fmt"
	"strconv"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var vpsCmd = &cobra.Command{
	Use:   "vps",
	Short: "VPS read commands (list / show / status). Write verbs land in 7.3.",
}

// ── vps list ─────────────────────────────────────────────────────

var (
	vpsListBackend string
	vpsListStatus  string
)

var vpsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every VPS on the account, across both backends.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		vs, err := c.VpsList(cmd.Context(), vpsListBackend, vpsListStatus)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), vs, f)
		}

		if len(vs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No VPSs on this account.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "backend", "product", "domain", "ip", "status", "next due"})
		for _, v := range vs {
			t.AppendRow(table.Row{
				v.ID,
				v.VpsBackend,
				v.Product,
				v.Domain,
				v.DedicatedIP,
				v.Status,
				v.NextDue,
			})
		}
		t.Render()
		return nil
	},
}

// ── vps show <id> ────────────────────────────────────────────────

var vpsShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one VPS by service id (backend resolved from service detail).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		v, err := c.VpsShow(cmd.Context(), id)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), v, f)
		}

		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"id", v.ID})
		t.AppendRow(table.Row{"backend", v.VpsBackend})
		t.AppendRow(table.Row{"product", v.Product})
		t.AppendRow(table.Row{"group", v.ProductGroup})
		t.AppendRow(table.Row{"domain", v.Domain})
		t.AppendRow(table.Row{"dedicated_ip", v.DedicatedIP})
		t.AppendRow(table.Row{"status", v.Status})
		t.AppendRow(table.Row{"billing_cycle", v.BillingCycle})
		t.AppendRow(table.Row{"amount", fmt.Sprintf("%.2f", v.Amount)})
		t.AppendRow(table.Row{"registered_at", v.RegisteredAt})
		t.AppendRow(table.Row{"next_due", v.NextDue})
		if v.Username != "" {
			t.AppendRow(table.Row{"username", v.Username})
		}
		if v.ServerHostname != "" {
			t.AppendRow(table.Row{"server_hostname", v.ServerHostname})
		}
		if v.ServerIP != "" {
			t.AppendRow(table.Row{"server_ip", v.ServerIP})
		}
		t.Render()
		return nil
	},
}

// ── vps status <id> ──────────────────────────────────────────────

var vpsStatusCmd = &cobra.Command{
	Use:   "status <id>",
	Short: "Show the live power state of a VPS (Proxmox includes CPU + memory + uptime).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("vps id must be an integer: %s", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		backend, s, err := c.VpsStatus(cmd.Context(), id)
		if err != nil {
			return err
		}

		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			payload := map[string]any{"backend": backend}
			if s != nil {
				payload["power_state"] = s.PowerState
				if s.CPUUsage > 0 {
					payload["cpu_usage"] = s.CPUUsage
				}
				if s.MemoryUsed > 0 {
					payload["memory_used"] = s.MemoryUsed
					payload["memory_total"] = s.MemoryTotal
				}
				if s.Uptime > 0 {
					payload["uptime_seconds"] = s.Uptime
				}
			}
			return renderJSONOrYAML(cmd.OutOrStdout(), payload, f)
		}

		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"backend", backend})
		t.AppendRow(table.Row{"power_state", s.PowerState})
		if s.Uptime > 0 {
			t.AppendRow(table.Row{"uptime_seconds", s.Uptime})
		}
		if s.CPUUsage > 0 {
			t.AppendRow(table.Row{"cpu_usage_pct", fmt.Sprintf("%.2f", s.CPUUsage)})
		}
		if s.MemoryUsed > 0 {
			t.AppendRow(table.Row{"memory_used_mb", s.MemoryUsed / (1024 * 1024)})
			t.AppendRow(table.Row{"memory_total_mb", s.MemoryTotal / (1024 * 1024)})
		}
		t.Render()
		return nil
	},
}

func init() {
	vpsListCmd.Flags().StringVar(&vpsListBackend, "backend", "",
		"Filter by backend: proxmox | cloud.")
	vpsListCmd.Flags().StringVar(&vpsListStatus, "status", "",
		"Filter by status (Active, Suspended, ...).")

	vpsCmd.AddCommand(vpsListCmd, vpsShowCmd, vpsStatusCmd)
	rootCmd.AddCommand(vpsCmd)
}
