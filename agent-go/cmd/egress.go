package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/egress"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/state"
	"github.com/spf13/cobra"
)

// impreza-agent egress host ...: the server owner's control over the host
// INPUT half of the egress baseline (containers reaching services of this
// host). Each change is saved in the agent state, applied at once and kept
// across reboots and reconciles. Only ports are printed, never addresses.
func init() {
	egressCmd := &cobra.Command{Use: "egress", Short: "Inspect the tenant egress baseline of this server."}
	host := &cobra.Command{Use: "host", Short: "Control which services of this host containers may reach."}

	host.AddCommand(&cobra.Command{Use: "status", Args: cobra.NoArgs, Short: "Show the host baseline and the operator policy.", RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := state.Ensure("")
		if err != nil {
			return err
		}
		policy, perr := egress.LoadHostPolicy(dir)
		v4, v6 := egress.HostStatus(dir)
		out := map[string]any{"policy": policy, "v4": v4, "v6": v6}
		if perr != nil {
			out["policy_error"] = perr.Error()
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}})

	change := func(use, short string, args cobra.PositionalArgs, edit func(*egress.HostPolicy, []string) error) *cobra.Command {
		return &cobra.Command{Use: use, Args: args, Short: short, RunE: func(cmd *cobra.Command, a []string) error {
			dir, err := state.Ensure("")
			if err != nil {
				return err
			}
			policy, err := egress.LoadHostPolicy(dir)
			if err != nil {
				return errors.New(err.Error() + "; fix or remove " + dir + "/egress-host-policy.json first")
			}
			if err := edit(&policy, a); err != nil {
				return err
			}
			if err := egress.SaveHostPolicy(dir, policy); err != nil {
				return err
			}
			// The change is journaled like the daemon's own applies.
			egress.Notify = cliNotify
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			applyErr := errors.Join(egress.Apply(ctx, dir), egress.Apply6(ctx, dir))
			fmt.Fprintln(cmd.OutOrStdout(), "Saved. It stays in force across reboots and agent updates.")
			if applyErr != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Not applied yet ("+applyErr.Error()+"); the agent applies it on its next reconcile.")
			}
			return nil
		}}
	}
	host.AddCommand(change("allow PROTO/PORT", "Let containers reach this host port (for example tcp/8080).", cobra.ExactArgs(1),
		func(p *egress.HostPolicy, a []string) error {
			n, err := egress.NormalizeHostPort(a[0])
			if err != nil {
				return err
			}
			p.Allow = append(p.Allow, n)
			return nil
		}))
	host.AddCommand(change("remove PROTO/PORT", "Remove an exception added with allow.", cobra.ExactArgs(1),
		func(p *egress.HostPolicy, a []string) error {
			n, err := egress.NormalizeHostPort(a[0])
			if err != nil {
				return err
			}
			kept := p.Allow[:0]
			found := false
			for _, e := range p.Allow {
				if e == n {
					found = true
					continue
				}
				kept = append(kept, e)
			}
			if !found {
				return errors.New(n + " is not an exception")
			}
			p.Allow = kept
			return nil
		}))
	host.AddCommand(change("disable", "Turn the host baseline off on this server (containers reach every host service again).", cobra.NoArgs,
		func(p *egress.HostPolicy, _ []string) error { p.Disabled = true; return nil }))
	host.AddCommand(change("enable", "Turn the host baseline back on.", cobra.NoArgs,
		func(p *egress.HostPolicy, _ []string) error { p.Disabled = false; return nil }))

	egressCmd.AddCommand(host)
	rootCmd.AddCommand(egressCmd)
}
