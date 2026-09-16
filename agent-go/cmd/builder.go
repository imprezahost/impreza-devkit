package cmd

import (
	"fmt"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	builder := &cobra.Command{Use: "builder", Short: "Manage opt-in controlled builds on this server."}
	builder.AddCommand(&cobra.Command{Use: "status", Args: cobra.NoArgs, Short: "Check activation and local prerequisites without changing them.", RunE: func(cmd *cobra.Command, _ []string) error {
		d := &executor.Docker{StateDir: state.DefaultDir()}
		enabled, err := d.ControlledBuildsEnabled()
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Controlled builds enabled: %t\n", enabled)
		return d.CheckControlledBuilds(cmd.Context())
	}})
	builder.AddCommand(&cobra.Command{Use: "prepare", Args: cobra.NoArgs, Short: "Download the pinned executor and enable controlled builds for future operations.", RunE: func(cmd *cobra.Command, _ []string) error {
		d := &executor.Docker{StateDir: state.DefaultDir()}
		if err := d.PrepareControlledBuilds(cmd.Context()); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Controlled builds enabled for future builds. Existing operations and applications are unchanged.")
		return nil
	}})
	builder.AddCommand(&cobra.Command{Use: "disable", Args: cobra.NoArgs, Short: "Disable controlled builds for future operations; active work keeps its recovery state.", RunE: func(cmd *cobra.Command, _ []string) error {
		d := &executor.Docker{StateDir: state.DefaultDir()}
		return d.DisableControlledBuilds()
	}})
	rootCmd.AddCommand(builder)
}
