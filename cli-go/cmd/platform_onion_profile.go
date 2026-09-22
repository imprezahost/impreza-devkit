package cmd

// `impreza platform onion-profile` — Tor v3 hardening tiers
// (standard | hardened | max) for a deployment's hidden service.
//
//	set — change the tier of an existing .onion (the address never changes)
//
// The same tiers are accepted at creation time via --onion-profile on
// `impreza deploy` and `impreza platform deploy`.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// validateOnionProfileFlag mirrors the server's 400s before any HTTP
// call: unknown tier, or a tier without onion exposure.
func validateOnionProfileFlag(profile string, onion bool) error {
	if profile == "" {
		return nil
	}
	if !sdkclient.ValidOnionProfileTier(profile) {
		return fmt.Errorf("invalid --onion-profile %q: must be standard, hardened or max", profile)
	}
	if !onion {
		return fmt.Errorf("--onion-profile requires --onion")
	}
	return nil
}

var platformOnionProfileCmd = &cobra.Command{
	Use:   "onion-profile",
	Short: "Manage the Tor v3 hardening tier of a deployment's .onion.",
}

var platformOnionProfileSetProfile string

var platformOnionProfileSetCmd = &cobra.Command{
	Use:   "set <deployment-id>",
	Short: "Change the hardening tier of a deployment's hidden service.",
	Long: `Change the hardening tier of an EXISTING Tor v3 hidden service.

Tiers:

  standard   intro-point rate limiting (the safe upstream default)
  hardened   tighter rate limits + max streams
  max        + experimental proof-of-work — one layer among several,
             never a guaranteed DDoS protection; refused when the host's
             Tor lacks the PoW module

The .onion address never changes. Fails with 404 when the deployment has
no onion service and 422 when the agent lacks onion-profile-v1 (update
the agent first).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if platformOnionProfileSetProfile == "" {
			return fmt.Errorf("--profile is required: standard | hardened | max")
		}
		if !sdkclient.ValidOnionProfileTier(platformOnionProfileSetProfile) {
			return fmt.Errorf("invalid --profile %q: must be standard, hardened or max", platformOnionProfileSetProfile)
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		out, err := c.PlatformSetOnionProfile(cmd.Context(), args[0], platformOnionProfileSetProfile)
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
		fmt.Fprintf(w, "Onion profile update enqueued. command_id=%s profile=%s\n", out.CommandID, out.Profile)
		if out.Note != "" {
			fmt.Fprintf(w, "Note: %s\n", out.Note)
		}
		return nil
	},
}

func init() {
	platformOnionProfileSetCmd.Flags().StringVar(&platformOnionProfileSetProfile, "profile", "", "Tier to apply: standard | hardened | max (required).")
	platformOnionProfileCmd.AddCommand(platformOnionProfileSetCmd)
	platformCmd.AddCommand(platformOnionProfileCmd)
}
