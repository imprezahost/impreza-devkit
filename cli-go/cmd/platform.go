package cmd

// `impreza platform` — Impreza Platform Apps surface.
//
// Three sub-commands map to the `/v1/platform/*` API:
//
//	apps         — browse the curated catalog
//	deployments  — manage app instances (list / show / deploy /
//	               uninstall / restart)
//	servers      — list managed servers, issue bootstrap tokens
//
// The platform command group lives behind its own root verb so it
// doesn't collide with `impreza vps` / `impreza dedicated` (different
// product surfaces).

import (
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// ─────────────────────────────────────────────────────────────────────
// root: `impreza platform`
// ─────────────────────────────────────────────────────────────────────

var platformCmd = &cobra.Command{
	Use:   "platform",
	Short: "Impreza Platform managed-apps surface (catalog, deployments, servers).",
	Long: `Impreza Platform: deploy curated apps onto managed servers.

A typical first deploy looks like:

  impreza platform apps list
  impreza platform servers list
  impreza platform deploy vaultwarden \
      --agent agt_xxxxxxxxxxxx \
      --domain vault.example.com \
      --onion \
      --var signups_allowed=true

Track progress:

  impreza platform deployments list
  impreza platform deployments show dpl_xxxxxxxxxxxx

Tear down:

  impreza platform deployments uninstall dpl_xxxxxxxxxxxx --purge-data --confirm
`,
}

// ─────────────────────────────────────────────────────────────────────
// apps
// ─────────────────────────────────────────────────────────────────────

var platformAppsCmd = &cobra.Command{
	Use:   "apps",
	Short: "Browse the curated app catalog.",
}

var (
	platformAppsListCategory string
	platformAppsListSearch   string
)

var platformAppsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List enabled apps in the catalog.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformListApps(cmd.Context(), sdkclient.AppListParams{
			Category: platformAppsListCategory,
			Search:   platformAppsListSearch,
		})
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"name", "version", "category", "onion", "letsencrypt", "description"})
		for _, a := range out.Apps {
			onion := "no"
			le := "no"
			if a.Supports != nil {
				if a.Supports.Onion {
					onion = "yes"
				}
				if a.Supports.LetsEncrypt {
					le = "yes"
				}
			}
			t.AppendRow(table.Row{a.Name, a.Version, a.Category, onion, le, truncate(a.Description, 60)})
		}
		t.Render()
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d app(s)\n", out.Total)
		return nil
	},
}

var platformAppsInfoCmd = &cobra.Command{
	Use:   "info <name>",
	Short: "Show full metadata + manifest for a single app.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformGetApp(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"name", out.Name})
		t.AppendRow(table.Row{"display_name", out.DisplayName})
		t.AppendRow(table.Row{"version", out.Version})
		t.AppendRow(table.Row{"category", out.Category})
		if len(out.Tags) > 0 {
			t.AppendRow(table.Row{"tags", strings.Join(out.Tags, ", ")})
		}
		if out.Description != "" {
			t.AppendRow(table.Row{"description", out.Description})
		}
		if out.Requires != nil {
			t.AppendRow(table.Row{"ram_mb", out.Requires.RAMMB})
			t.AppendRow(table.Row{"disk_gb", out.Requires.DiskGB})
			t.AppendRow(table.Row{"cpu_cores", out.Requires.CPUCores})
		}
		if out.Supports != nil {
			t.AppendRow(table.Row{"supports.onion", boolStr(out.Supports.Onion)})
			t.AppendRow(table.Row{"supports.custom_domain", boolStr(out.Supports.CustomDomain)})
			t.AppendRow(table.Row{"supports.letsencrypt", boolStr(out.Supports.LetsEncrypt)})
		}
		if out.ReadmeURL != "" {
			t.AppendRow(table.Row{"readme_url", out.ReadmeURL})
		}
		t.Render()
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// deploy (top-level shortcut for the create-deployment endpoint)
// ─────────────────────────────────────────────────────────────────────

var (
	platformDeployAgent    string
	platformDeployVersion  string
	platformDeployDomain   string
	platformDeployOnion    bool
	platformDeployVarFlags []string
)

var platformDeployCmd = &cobra.Command{
	Use:   "deploy <app>",
	Short: "Deploy a catalog app to one of your managed servers.",
	Long: `Deploys <app> from the catalog to the specified agent. Returns
the new deployment id; poll progress via:

  impreza platform deployments show <id>

Variables are passed with --var KEY=VALUE (repeatable). Required vars
(declared in the manifest) MUST be supplied; defaults apply to the
rest. Common ones for Vaultwarden: --var host_port=8080 --var signups_allowed=true.

When --domain is set, the agent's Caddy provisions Let's Encrypt for
that hostname (DNS must already point at the VPS). --onion adds a
Tor v3 hidden service mirror for the same upstream.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if platformDeployAgent == "" {
			return fmt.Errorf("--agent is required (try: impreza platform servers list)")
		}
		vars := map[string]any{}
		for _, kv := range platformDeployVarFlags {
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				return fmt.Errorf("--var must be KEY=VALUE (got %q)", kv)
			}
			vars[kv[:eq]] = kv[eq+1:]
		}
		req := sdkclient.DeploymentCreateRequest{
			AppName:    args[0],
			AppVersion: platformDeployVersion,
			AgentID:    platformDeployAgent,
			Vars:       vars,
			Domain:     platformDeployDomain,
			Onion:      platformDeployOnion,
		}
		out, err := c.PlatformCreateDeployment(cmd.Context(), req)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Deployment created.\n")
		printDeployment(cmd, out)
		fmt.Fprintf(cmd.OutOrStdout(),
			"\nPoll progress with: impreza platform deployments show %s\n", out.ID)
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// deployments
// ─────────────────────────────────────────────────────────────────────

var platformDeploymentsCmd = &cobra.Command{
	Use:   "deployments",
	Short: "Manage app instances (list / show / uninstall / restart / redeploy).",
}

var (
	platformDeploymentsListAgent  string
	platformDeploymentsListStatus string
)

var platformDeploymentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List deployments owned by the authenticated client.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformListDeployments(cmd.Context(), sdkclient.DeploymentListParams{
			AgentID: platformDeploymentsListAgent,
			Status:  platformDeploymentsListStatus,
		})
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"id", "app", "version", "agent", "status", "domain", "onion"})
		for _, d := range out.Deployments {
			t.AppendRow(table.Row{
				d.ID, d.AppName, d.AppVersion, d.AgentID,
				string(d.Status),
				orDash(d.Domain),
				orDash(d.Onion),
			})
		}
		t.Render()
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d deployment(s)\n", out.Total)
		return nil
	},
}

var platformDeploymentsShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one deployment with full state.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformGetDeployment(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		printDeployment(cmd, out)
		return nil
	},
}

var (
	platformDeploymentsUninstallPurge   bool
	platformDeploymentsUninstallConfirm bool
)

var platformDeploymentsUninstallCmd = &cobra.Command{
	Use:   "uninstall <id>",
	Short: "Uninstall a deployment (stop containers, remove Caddy / Tor routes).",
	Long: `Enqueues an uninstall command for the deployment's agent. The agent
runs ` + "`docker compose down`" + ` for the deployment, removes its Caddy + Tor
route fragments, and reloads both. Persistent volumes are preserved
unless --purge-data is set.

Requires --confirm because the operation is irreversible at the
container level (unless data is purged, restarting the same deployment_id
is impossible — uninstall is terminal).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !platformDeploymentsUninstallConfirm {
			return fmt.Errorf("uninstall is destructive — re-run with --confirm")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformUninstall(cmd.Context(), args[0], sdkclient.PlatformUninstallRequest{
			PurgeData: platformDeploymentsUninstallPurge,
			Confirm:   true,
		})
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Uninstall enqueued. command_id=%s\n", out.CommandID)
		printDeployment(cmd, &out.Deployment)
		return nil
	},
}

var platformDeploymentsRestartCmd = &cobra.Command{
	Use:   "restart <id>",
	Short: "Restart a deployment's containers (docker compose restart).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformRestart(cmd.Context(), args[0], sdkclient.PlatformRestartRequest{})
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Restart enqueued. command_id=%s\n", out.CommandID)
		printDeployment(cmd, &out.Deployment)
		return nil
	},
}

var (
	platformDeploymentsRedeployEnv    []string
	platformDeploymentsRedeployFollow bool
)

var platformDeploymentsRedeployCmd = &cobra.Command{
	Use:   "redeploy <id>",
	Short: "Rebuild a custom deployment in place (same domain, no teardown).",
	Long: `Rebuild a custom deployment from its CURRENT source on the same
server — re-pull the image, re-clone the watched git ref at its new
HEAD, or rebuild — and swap the container with near-zero downtime.

The deployment_id, domain, and host port are preserved, so the app's
URL never changes. This is the in-place alternative to uninstall +
recreate: use it to ship a new build of a running custom app.

  impreza platform deployments redeploy dpl_xxxxxxxxxxxx --follow

--env KEY=VALUE flags (repeatable) merge into the deployment's stored
environment before the rebuild — rotate a secret or add a var without a
teardown. System vars (DEPLOYMENT_ID, DOMAIN_URL, HOST_PORT, ...) are
preserved.

The source itself isn't changed here (a redeploy ships the current
source). To change the image ref or git URL, uninstall + recreate under
the SAME name — your *.imprezaapps.com domain is preserved either way.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		vars := map[string]any{}
		for _, kv := range platformDeploymentsRedeployEnv {
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				return fmt.Errorf("--env must be KEY=VALUE (got %q)", kv)
			}
			vars[kv[:eq]] = kv[eq+1:]
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		req := sdkclient.CustomRedeployRequest{}
		if len(vars) > 0 {
			req.Vars = vars
		}
		out, err := c.PlatformRedeployCustomDeployment(cmd.Context(), args[0], req)
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Redeploy enqueued. command_id=%s\n", out.CommandID)
		if out.Domain != "" {
			fmt.Fprintf(w, "Domain (unchanged): %s\n", out.Domain)
		}
		if !platformDeploymentsRedeployFollow {
			fmt.Fprintf(w, "Status: %s\nTrack progress with:\n  impreza platform deployments show %s\n", out.Status, out.ID)
			return nil
		}
		steps := NewStepper(w, 1)
		steps.Step("Waiting for status to settle (--follow)")
		final, err := pollUntilSettled(cmd, c, out.ID, steps)
		if err != nil {
			return err
		}
		rows := []KV{
			{Key: "id", Value: final.ID},
			{Key: "status", Value: string(final.Status)},
		}
		if final.Domain != "" {
			rows = append(rows, KV{Key: "url", Value: final.Domain})
		}
		if final.Onion != "" {
			rows = append(rows, KV{Key: "onion", Value: final.Onion})
		}
		steps.Banner("✓ Redeployed", rows)
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// servers
// ─────────────────────────────────────────────────────────────────────

var platformServersCmd = &cobra.Command{
	Use:   "servers",
	Short: "Manage Impreza Platform servers (impreza-managed + bring-your-own).",
}

var platformServersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List managed servers visible to the authenticated client.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformListServers(cmd.Context())
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"agent_id", "hostname", "origin", "status", "version", "last_seen_at"})
		for _, s := range out.Servers {
			lastSeen := ""
			if s.LastSeenAt != nil {
				lastSeen = s.LastSeenAt.UTC().Format("2006-01-02 15:04:05")
			}
			t.AppendRow(table.Row{s.AgentID, s.Hostname, string(s.Origin), string(s.Status), s.Version, lastSeen})
		}
		t.Render()
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d server(s)\n", out.Total)
		return nil
	},
}

var platformServersBootstrapLabel string

var platformServersBootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Issue a one-time bootstrap token + curl|sh one-liner for a bring-your-own server.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformIssueExternalBootstrap(cmd.Context(), sdkclient.ExternalBootstrapRequest{
			Label: platformServersBootstrapLabel,
		})
		if err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), out, f)
		}
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"bootstrap_token", out.BootstrapToken})
		t.AppendRow(table.Row{"expires_at", out.ExpiresAt.UTC().Format("2006-01-02 15:04:05Z")})
		t.AppendRow(table.Row{"install_url", out.InstallURL})
		t.Render()
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "Run this on your VPS (as root):")
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "  "+out.OneLiner)
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// Helpers (file-local; bigger ones live in helpers.go)
// ─────────────────────────────────────────────────────────────────────

func printDeployment(cmd *cobra.Command, d *sdkclient.Deployment) {
	t := output.NewTable(cmd.OutOrStdout())
	t.AppendHeader(table.Row{"field", "value"})
	t.AppendRow(table.Row{"id", d.ID})
	t.AppendRow(table.Row{"app", fmt.Sprintf("%s %s", d.AppName, d.AppVersion)})
	t.AppendRow(table.Row{"agent_id", d.AgentID})
	t.AppendRow(table.Row{"status", string(d.Status)})
	if d.Domain != "" {
		t.AppendRow(table.Row{"domain", d.Domain})
	}
	if d.Onion != "" {
		t.AppendRow(table.Row{"onion", d.Onion})
	}
	if d.LastError != "" {
		t.AppendRow(table.Row{"last_error", d.LastError})
	}
	t.AppendRow(table.Row{"created_at", d.CreatedAt.UTC().Format("2006-01-02 15:04:05Z")})
	if d.LastHealthAt != nil {
		t.AppendRow(table.Row{"last_health_at", d.LastHealthAt.UTC().Format("2006-01-02 15:04:05Z")})
	}
	t.Render()
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ─────────────────────────────────────────────────────────────────────
// Wire-up
// ─────────────────────────────────────────────────────────────────────

func init() {
	// apps
	platformAppsListCmd.Flags().StringVar(&platformAppsListCategory, "category", "", "Filter by category (security, communication, ...).")
	platformAppsListCmd.Flags().StringVar(&platformAppsListSearch, "search", "", "Substring match on name + display_name + description.")
	platformAppsCmd.AddCommand(platformAppsListCmd, platformAppsInfoCmd)

	// deploy (top-level shortcut)
	platformDeployCmd.Flags().StringVar(&platformDeployAgent, "agent", "", "agent_id of the target server (required).")
	platformDeployCmd.Flags().StringVar(&platformDeployVersion, "version", "", "App version (default: latest).")
	platformDeployCmd.Flags().StringVar(&platformDeployDomain, "domain", "", "Public hostname for TLS / Let's Encrypt (DNS must point at the VPS).")
	platformDeployCmd.Flags().BoolVar(&platformDeployOnion, "onion", false, "Also publish a Tor v3 hidden service.")
	platformDeployCmd.Flags().StringArrayVar(&platformDeployVarFlags, "var", nil, "KEY=VALUE manifest variable (repeatable).")
	_ = platformDeployCmd.MarkFlagRequired("agent")

	// deployments
	platformDeploymentsListCmd.Flags().StringVar(&platformDeploymentsListAgent, "agent", "", "Filter by agent_id.")
	platformDeploymentsListCmd.Flags().StringVar(&platformDeploymentsListStatus, "status", "", "Filter by status (running, failed, ...).")
	platformDeploymentsUninstallCmd.Flags().BoolVar(&platformDeploymentsUninstallPurge, "purge-data", false, "Also wipe persistent volumes (docker compose down --volumes).")
	platformDeploymentsUninstallCmd.Flags().BoolVar(&platformDeploymentsUninstallConfirm, "confirm", false, "Required gate — uninstall is destructive.")
	platformDeploymentsRedeployCmd.Flags().StringArrayVar(&platformDeploymentsRedeployEnv, "env", nil, "KEY=VALUE env var merged into the deployment before the rebuild (repeatable).")
	platformDeploymentsRedeployCmd.Flags().BoolVar(&platformDeploymentsRedeployFollow, "follow", false, "Block until the redeploy leaves updating/installing.")
	platformDeploymentsCmd.AddCommand(
		platformDeploymentsListCmd,
		platformDeploymentsShowCmd,
		platformDeploymentsUninstallCmd,
		platformDeploymentsRestartCmd,
		platformDeploymentsRedeployCmd,
	)

	// servers
	platformServersBootstrapCmd.Flags().StringVar(&platformServersBootstrapLabel, "label", "", "Friendly label for the panel (optional).")
	platformServersCmd.AddCommand(platformServersListCmd, platformServersBootstrapCmd)

	// platform root
	platformCmd.AddCommand(
		platformAppsCmd,
		platformDeployCmd,
		platformDeploymentsCmd,
		platformServersCmd,
	)
	rootCmd.AddCommand(platformCmd)
}
