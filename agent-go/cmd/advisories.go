package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/config"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/scanner"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	command := &cobra.Command{Use: "advisories", Short: "Inspect or update the signed dependency advisory database."}
	command.AddCommand(&cobra.Command{Use: "update", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load(globalConfigPath)
		if err != nil {
			return err
		}
		dir, err := state.Ensure("")
		if err != nil {
			return err
		}
		if err = scanner.UpdateAdvisories(cmd.Context(), dir, cfg.UseTor, cfg.Proxy); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Signed advisory database verified and current.")
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := state.Ensure("")
		if err != nil {
			return err
		}
		status := map[string]any{"available": false}
		if key, err := scanner.TrustedAdvisoryKey(); err == nil {
			status["trusted_key_sha256"] = fmt.Sprintf("%x", sha256.Sum256(key))
		}
		b := scanner.LoadAdvisoryBase(dir)
		if b == nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		}
		status["available"], status["revision"], status["generated_at"] = true, b.Revision, b.GeneratedAt
		status["expires_at"], status["incomplete"], status["entries"] = b.ExpiresAt, b.Incomplete, len(b.Entries)
		return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
	}})
	rootCmd.AddCommand(command)
}
