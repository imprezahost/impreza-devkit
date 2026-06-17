// Dedicated server commands.
//
// Surface mirrors the public `/dedicated/*` namespace exposed by the
// imprezaapi addon. Operations are gated by per-service capabilities —
// calling a capability-gated sub-command against a service that doesn't
// advertise the capability returns `NOT_SUPPORTED`.
//
// Verb conventions follow `impreza vps`: list / show / status / start /
// shutdown / reboot. Capability-gated reads (firewall / bandwidth / vpn)
// surface as their own sub-commands.
//
// Reinstall is destructive: the wrapper requires `--confirm` and prompts
// (unless `--yes` is passed). The X-Impreza-Confirm: WIPE header is
// injected by the internal client.

package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var dedicatedCmd = &cobra.Command{
	Use:   "dedicated",
	Short: "Manage dedicated servers. Operations gated by per-service capabilities.",
}

// ── dedicated list ──────────────────────────────────────────────────

var dedicatedListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every dedicated server on the account.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		ds, err := c.DedicatedList(cmd.Context())
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
			fmt.Fprintln(cmd.OutOrStdout(), "No dedicated servers on this account.")
			return nil
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"service_id", "domain", "ip", "status", "capabilities"})
		for _, d := range ds {
			t.AppendRow(table.Row{
				d.ServiceID,
				d.Domain,
				d.IP,
				d.Status,
				strings.Join(d.Capabilities, ","),
			})
		}
		t.Render()
		return nil
	},
}

// ── dedicated show <id> ─────────────────────────────────────────────

var dedicatedShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show full details for a dedicated server.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		v, err := c.DedicatedShow(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		// `show` returns a heterogeneous map (shape varies per service)
		// so the table view is intentionally a flat key=value dump rather
		// than a typed projection — JSON/YAML stays the canonical surface.
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), v, f)
		}
		return renderJSONOrYAML(cmd.OutOrStdout(), v, output.FormatJSON)
	},
}

// ── dedicated capabilities <id> ─────────────────────────────────────

var dedicatedCapsCmd = &cobra.Command{
	Use:   "capabilities <id>",
	Short: "Show the capability list for a dedicated server.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		caps, err := c.DedicatedCapabilities(cmd.Context(), id)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), caps, f)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "capabilities:")
		for _, c := range caps.Capabilities {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", c)
		}
		return nil
	},
}

// ── dedicated status / ips / os-images / kvm / vpn / firewall / bandwidth ──
// All five wrap a single Get call and dump the response as JSON in the
// non-table format. Table view is shallow key=value because the response
// shape varies per service.

var dedicatedStatusCmd = simpleDedicatedGet(
	"status <id>",
	"Show current power / provisioning state.",
	func(ctx context.Context, c *client.Client, id int) (any, error) {
		return c.DedicatedStatus(ctx, id)
	},
)

var dedicatedIpsCmd = simpleDedicatedGet(
	"ips <id>",
	"List the IPs assigned to the server with current PTR.",
	func(ctx context.Context, c *client.Client, id int) (any, error) {
		return c.DedicatedIps(ctx, id)
	},
)

var dedicatedOsImagesCmd = simpleDedicatedGet(
	"os-images <id>",
	"List OS images available for reinstall.",
	func(ctx context.Context, c *client.Client, id int) (any, error) {
		return c.DedicatedOsImages(ctx, id)
	},
)

var dedicatedKvmCmd = simpleDedicatedGet(
	"kvm <id>",
	"Show current KVM / IPMI access info.",
	func(ctx context.Context, c *client.Client, id int) (any, error) {
		return c.DedicatedKvm(ctx, id)
	},
)

var dedicatedVpnCmd = simpleDedicatedGet(
	"vpn <id>",
	"Show rotating VPN credentials (requires the `vpn` capability).",
	func(ctx context.Context, c *client.Client, id int) (any, error) {
		return c.DedicatedVpn(ctx, id)
	},
)

var dedicatedFirewallCmd = simpleDedicatedGet(
	"firewall <id>",
	"Show DDoS firewall state (requires the `firewall` capability).",
	func(ctx context.Context, c *client.Client, id int) (any, error) {
		return c.DedicatedFirewall(ctx, id)
	},
)

var dedicatedDdosLogsCmd = simpleDedicatedGet(
	"ddos-logs <id>",
	"Show DDoS attack logs (requires the `firewall` capability).",
	func(ctx context.Context, c *client.Client, id int) (any, error) {
		return c.DedicatedDdosLogs(ctx, id)
	},
)

var (
	dedicatedBandwidthType  string
	dedicatedBandwidthScale string
)

var dedicatedBandwidthCmd = &cobra.Command{
	Use:   "bandwidth <id>",
	Short: "Bandwidth graph (PNG base64, requires the `bandwidth` capability).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		bw, err := c.DedicatedBandwidth(cmd.Context(), id, dedicatedBandwidthType, dedicatedBandwidthScale)
		if err != nil {
			return err
		}
		return renderJSONOrYAML(cmd.OutOrStdout(), bw, output.FormatJSON)
	},
}

// ── dedicated start / shutdown / reboot ─────────────────────────────

var dedicatedStartCmd = &cobra.Command{
	Use:   "start <id>",
	Short: "Power on a dedicated server.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DedicatedStart(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("Dedicated server %d start signal sent.", id)
		return nil
	},
}

var dedicatedShutdownCmd = &cobra.Command{
	Use:   "shutdown <id>",
	Short: "Graceful shutdown.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DedicatedShutdown(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("Dedicated server %d shutdown signal sent.", id)
		return nil
	},
}

var dedicatedRebootCmd = &cobra.Command{
	Use:   "reboot <id>",
	Short: "Reboot.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DedicatedReboot(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("Dedicated server %d reboot signal sent.", id)
		return nil
	},
}

// ── dedicated set-rdns / reset-rdns ─────────────────────────────────

var (
	dedicatedRdnsIP       string
	dedicatedRdnsHostname string
)

var dedicatedSetRdnsCmd = &cobra.Command{
	Use:   "set-rdns <id>",
	Short: "Set the PTR for a single IP on a dedicated server.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		if dedicatedRdnsIP == "" || dedicatedRdnsHostname == "" {
			return fmt.Errorf("--ip and --hostname are required")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.DedicatedSetRdns(cmd.Context(), id, dedicatedRdnsIP, dedicatedRdnsHostname)
		if err != nil {
			return err
		}
		return renderJSONOrYAML(cmd.OutOrStdout(), out, output.FormatJSON)
	},
}

var dedicatedResetRdnsCmd = &cobra.Command{
	Use:   "reset-rdns <id>",
	Short: "Reset every PTR back to the Impreza default (impreza.host pattern).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.DedicatedResetRdns(cmd.Context(), id)
		if err != nil {
			return err
		}
		return renderJSONOrYAML(cmd.OutOrStdout(), out, output.FormatJSON)
	},
}

// ── dedicated enable-kvm / disable-kvm ─────────────────────────────

var dedicatedEnableKvmCmd = &cobra.Command{
	Use:   "enable-kvm <id>",
	Short: "Open a KVM/IPMI session (calling IP auto-injected when the service needs it).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.DedicatedEnableKvm(cmd.Context(), id)
		if err != nil {
			return err
		}
		return renderJSONOrYAML(cmd.OutOrStdout(), out, output.FormatJSON)
	},
}

var dedicatedDisableKvmCmd = &cobra.Command{
	Use:   "disable-kvm <id>",
	Short: "Close the active KVM/IPMI session.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.DedicatedDisableKvm(cmd.Context(), id); err != nil {
			return err
		}
		output.Success("KVM session for service %d disabled.", id)
		return nil
	},
}

// ── dedicated set-firewall ──────────────────────────────────────────

var (
	dedicatedFwIP          string
	dedicatedFwState       string
	dedicatedFwSensitivity string
)

var dedicatedSetFirewallCmd = &cobra.Command{
	Use:   "set-firewall <id>",
	Short: "Update DDoS firewall state/sensitivity for an IP (requires the `firewall` capability).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		if dedicatedFwIP == "" {
			return fmt.Errorf("--ip is required")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		req := client.SetFirewallRequest{IP: dedicatedFwIP}
		if dedicatedFwState != "" {
			s := dedicatedFwState
			req.State = &s
		}
		if dedicatedFwSensitivity != "" {
			s := dedicatedFwSensitivity
			req.Sensitivity = &s
		}
		out, err := c.DedicatedSetFirewall(cmd.Context(), id, req)
		if err != nil {
			return err
		}
		return renderJSONOrYAML(cmd.OutOrStdout(), out, output.FormatJSON)
	},
}

// ── dedicated reinstall ─────────────────────────────────────────────

var (
	dedicatedReinstallOsID     string
	dedicatedReinstallOsLabel  string
	dedicatedReinstallPassword string
	dedicatedReinstallConfirm  bool
)

var dedicatedReinstallCmd = &cobra.Command{
	Use:   "reinstall <id>",
	Short: "Reinstall the OS. Destructive — wipes ALL data.",
	Long: `Reinstall the OS on a dedicated server. Destructive — wipes ALL data.

On services that support a synchronous reinstall path, the result
returns immediately and includes the new root password. On services
where the reinstall has to be applied manually, the response is
{status: "queued", message: "..."} and our team executes the reinstall
within a few hours.

Both --confirm AND the prompt (or --yes) must pass. The required
X-Impreza-Confirm: WIPE header is injected by the CLI automatically.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := parseServiceID(args[0])
		if err != nil {
			return err
		}
		if !dedicatedReinstallConfirm {
			return fmt.Errorf("reinstall is destructive — pass --confirm to acknowledge data loss")
		}
		if dedicatedReinstallOsID == "" || dedicatedReinstallPassword == "" {
			return fmt.Errorf("--os-id and --password are required")
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Reinstall service %d to os-id=%s? ALL DATA WILL BE LOST.", id, dedicatedReinstallOsID),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		req := client.ReinstallRequest{
			OsID:     dedicatedReinstallOsID,
			OsLabel:  dedicatedReinstallOsLabel,
			Password: dedicatedReinstallPassword,
			Confirm:  true,
		}
		out, err := c.DedicatedReinstall(cmd.Context(), id, req)
		if err != nil {
			return err
		}
		return renderJSONOrYAML(cmd.OutOrStdout(), out, output.FormatJSON)
	},
}

// ── helpers ─────────────────────────────────────────────────────────

// parseServiceID is the standard "first positional arg must be int" parse
// used by every `dedicated <verb> <id>` subcommand.
func parseServiceID(arg string) (int, error) {
	id, err := strconv.Atoi(arg)
	if err != nil {
		return 0, fmt.Errorf("service id must be an integer: %s", arg)
	}
	return id, nil
}

// simpleDedicatedGet wires the boilerplate for a read sub-command that
// takes a single <id> arg and emits the upstream JSON payload as-is.
// Backend responses are heterogeneous per service, so we render JSON
// regardless of the global --output flag — falling back to a flat
// key=value table would just hide structure for no gain.
func simpleDedicatedGet(use, short string, fn func(context.Context, *client.Client, int) (any, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseServiceID(args[0])
			if err != nil {
				return err
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			out, err := fn(cmd.Context(), c, id)
			if err != nil {
				return err
			}
			return renderJSONOrYAML(cmd.OutOrStdout(), out, output.FormatJSON)
		},
	}
}

func init() {
	dedicatedBandwidthCmd.Flags().StringVar(&dedicatedBandwidthType, "type", "port_bits",
		"port_bits | port_upkts | port_percent | port_errors | port_pktsize | port_discards")
	dedicatedBandwidthCmd.Flags().StringVar(&dedicatedBandwidthScale, "scale", "month",
		"day | week | month")

	dedicatedSetRdnsCmd.Flags().StringVar(&dedicatedRdnsIP, "ip", "", "IP whose PTR you want to set (required).")
	dedicatedSetRdnsCmd.Flags().StringVar(&dedicatedRdnsHostname, "hostname", "", "New PTR value (required).")

	dedicatedSetFirewallCmd.Flags().StringVar(&dedicatedFwIP, "ip", "", "IP to update (required).")
	dedicatedSetFirewallCmd.Flags().StringVar(&dedicatedFwState, "state", "",
		"always_on | redirect_on_attack (omit to leave unchanged).")
	dedicatedSetFirewallCmd.Flags().StringVar(&dedicatedFwSensitivity, "sensitivity", "",
		"low | normal | medium | high (omit to leave unchanged).")

	dedicatedReinstallCmd.Flags().StringVar(&dedicatedReinstallOsID, "os-id", "", "OS id from `dedicated os-images <id>` (required).")
	dedicatedReinstallCmd.Flags().StringVar(&dedicatedReinstallOsLabel, "os-label", "", "OS label (optional — auto-resolved from os-id).")
	dedicatedReinstallCmd.Flags().StringVar(&dedicatedReinstallPassword, "password", "", "New root password (required, min 8 chars).")
	dedicatedReinstallCmd.Flags().BoolVar(&dedicatedReinstallConfirm, "confirm", false, "Acknowledge that ALL DATA will be wiped.")
	dedicatedReinstallCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the interactive confirmation prompt.")

	dedicatedCmd.AddCommand(
		dedicatedListCmd,
		dedicatedShowCmd,
		dedicatedCapsCmd,
		dedicatedStatusCmd,
		dedicatedIpsCmd,
		dedicatedOsImagesCmd,
		dedicatedKvmCmd,
		dedicatedVpnCmd,
		dedicatedFirewallCmd,
		dedicatedDdosLogsCmd,
		dedicatedBandwidthCmd,
		dedicatedStartCmd,
		dedicatedShutdownCmd,
		dedicatedRebootCmd,
		dedicatedSetRdnsCmd,
		dedicatedResetRdnsCmd,
		dedicatedEnableKvmCmd,
		dedicatedDisableKvmCmd,
		dedicatedSetFirewallCmd,
		dedicatedReinstallCmd,
	)
	rootCmd.AddCommand(dedicatedCmd)
}
