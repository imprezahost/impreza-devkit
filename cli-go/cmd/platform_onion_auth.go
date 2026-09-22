package cmd

// `impreza platform onion-auth` — Tor v3 restricted discovery
// (client authorization) for a deployment's hidden service.
//
//	list   — show whether the service is restricted + authorized clients
//	add    — authorize a client (its own pubkey, or a generated keypair)
//	revoke — remove one client's authorization
//
// Key custody: with --pubkey the client's private key never leaves the
// customer's Tor client. With --generate the server mints the keypair
// and the private key is printed ONCE — never stored, never retrievable.

import (
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var platformOnionAuthCmd = &cobra.Command{
	Use:   "onion-auth",
	Short: "Manage Tor v3 restricted discovery (client authorization) on a deployment's .onion.",
}

var platformOnionAuthListCmd = &cobra.Command{
	Use:   "list <deployment-id>",
	Short: "List authorized onion clients and whether the service is restricted.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformOnionAuthList(cmd.Context(), args[0])
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
		fmt.Fprintf(cmd.OutOrStdout(), "Restricted: %s\n", boolStr(out.Restricted))
		t := output.NewTable(cmd.OutOrStdout())
		t.AppendHeader(table.Row{"name", "created_at"})
		for _, cl := range out.Clients {
			t.AppendRow(table.Row{cl.Name, cl.CreatedAt.UTC().Format("2006-01-02 15:04:05Z")})
		}
		t.Render()
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d client(s)\n", len(out.Clients))
		return nil
	},
}

var (
	platformOnionAuthAddName     string
	platformOnionAuthAddPubkey   string
	platformOnionAuthAddGenerate bool
)

var platformOnionAuthAddCmd = &cobra.Command{
	Use:   "add <deployment-id>",
	Short: "Authorize a client on the deployment's restricted onion service.",
	Long: `Authorize a client by NAME on a Tor v3 restricted-discovery hidden service.

Two modes — pick exactly one:

  --pubkey <x25519-pub>   the customer generated the keypair in their OWN
                          Tor client; the private key never leaves their
                          machine and is never sent to or stored by Impreza.
  --generate              the server mints the keypair; the private key is
                          printed ONCE below — save it immediately, it is
                          never stored and cannot be retrieved later.

A duplicate name is refused (409); revoke first to re-use it. Requires an
agent supporting onion-auth-v1 (422 on older agents).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if platformOnionAuthAddName == "" {
			return fmt.Errorf("--name is required")
		}
		if platformOnionAuthAddGenerate && platformOnionAuthAddPubkey != "" {
			return fmt.Errorf("pass either --pubkey or --generate, not both")
		}
		if !platformOnionAuthAddGenerate && platformOnionAuthAddPubkey == "" {
			return fmt.Errorf("pass --pubkey, or --generate to mint the keypair server-side")
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformOnionAuthAdd(cmd.Context(), args[0], sdkclient.OnionAuthAddRequest{
			Name:     platformOnionAuthAddName,
			Pubkey:   platformOnionAuthAddPubkey,
			Generate: platformOnionAuthAddGenerate,
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
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Client authorized.\n")
		t := output.NewTable(w)
		t.AppendHeader(table.Row{"field", "value"})
		t.AppendRow(table.Row{"name", out.Name})
		t.AppendRow(table.Row{"pubkey", out.Pubkey})
		if out.PrivateKey != "" {
			t.AppendRow(table.Row{"private_key", out.PrivateKey})
		}
		t.Render()
		if out.PrivateKey != "" {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "SAVE THIS PRIVATE KEY NOW — shown exactly once, never stored, cannot be retrieved later.")
			fmt.Fprintln(w, "If it is lost: impreza platform onion-auth revoke "+args[0]+" --name "+out.Name+", then add again.")
		}
		return nil
	},
}

var platformOnionAuthRevokeName string

var platformOnionAuthRevokeCmd = &cobra.Command{
	Use:   "revoke <deployment-id>",
	Short: "Revoke one client's authorization from the restricted onion service.",
	Long: `Revoke the client NAME from a Tor v3 restricted-discovery hidden service.
The client loses access immediately; the deployment, the .onion address and
every other client are untouched. The name can be authorized again later
with a fresh keypair.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if platformOnionAuthRevokeName == "" {
			return fmt.Errorf("--name is required (try: impreza platform onion-auth list %s)", args[0])
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.PlatformOnionAuthRevoke(cmd.Context(), args[0], platformOnionAuthRevokeName); err != nil {
			return err
		}
		f, err := resolveFormat()
		if err != nil {
			return err
		}
		if f != output.FormatTable {
			return renderJSONOrYAML(cmd.OutOrStdout(), map[string]any{"revoked": platformOnionAuthRevokeName}, f)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Client %q revoked from %s.\n", platformOnionAuthRevokeName, args[0])
		return nil
	},
}

func init() {
	platformOnionAuthAddCmd.Flags().StringVar(&platformOnionAuthAddName, "name", "", "Client label, unique within the deployment (required).")
	platformOnionAuthAddCmd.Flags().StringVar(&platformOnionAuthAddPubkey, "pubkey", "", "Client's own x25519 public key (mutually exclusive with --generate).")
	platformOnionAuthAddCmd.Flags().BoolVar(&platformOnionAuthAddGenerate, "generate", false, "Mint the keypair server-side; the private key is shown once.")
	platformOnionAuthRevokeCmd.Flags().StringVar(&platformOnionAuthRevokeName, "name", "", "Client name to revoke (required).")

	platformOnionAuthCmd.AddCommand(
		platformOnionAuthListCmd,
		platformOnionAuthAddCmd,
		platformOnionAuthRevokeCmd,
	)
	platformCmd.AddCommand(platformOnionAuthCmd)
}
