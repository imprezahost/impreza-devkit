package cmd

import (
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	"github.com/spf13/cobra"
)

func init() {
	var stateDir, workID string
	worker := &cobra.Command{Use: "replacement-worker", Hidden: true, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return executor.RunReplacementWorker(stateDir, workID) }}
	worker.Flags().StringVar(&stateDir, "state-dir", "", "Private agent state directory")
	worker.Flags().StringVar(&workID, "work-id", "", "Exact replacement work identity")
	rootCmd.AddCommand(worker)
}
