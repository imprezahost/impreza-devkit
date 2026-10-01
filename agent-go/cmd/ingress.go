package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/ingress"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	cmd := &cobra.Command{Use: "ingress", Short: "Inspect or recover the per-deployment ingress allowlists on this server."}
	cmd.AddCommand(&cobra.Command{Use: "status", Args: cobra.NoArgs, Short: "Show the stored revision and enforcement of each deployment (no sources are printed).", RunE: func(c *cobra.Command, _ []string) error {
		statuses, err := ingress.NewManager(state.DefaultDir()).Report()
		if err != nil {
			return err
		}
		if len(statuses) == 0 {
			fmt.Fprintln(c.OutOrStdout(), "No ingress allowlist on this server: every published port is open.")
			return nil
		}
		for _, s := range statuses {
			state := "enforced"
			if !s.Enforced {
				state = "not enforced (" + s.Reason + ")"
			}
			fmt.Fprintf(c.OutOrStdout(), "%s revision %d: %s\n", s.DeploymentID, s.Revision, state)
		}
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "reset", Args: cobra.NoArgs, Short: "Recovery: remove every ingress chain now, opening all published ports until the platform sends a new revision.", RunE: func(c *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
		defer cancel()
		if err := ingress.NewManager(state.DefaultDir()).Reset(ctx); err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), "Ingress chains removed: every published port is open. The dashboard reports the allowlists as not enforced until you save them again.")
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "restore", Args: cobra.NoArgs, Hidden: true, Short: "Boot: render the stored allowlists before Docker starts (run by impreza-agent-ingress.service).", RunE: func(c *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
		defer cancel()
		return ingress.NewManager(state.DefaultDir()).Restore(ctx)
	}})
	rootCmd.AddCommand(cmd)
}
