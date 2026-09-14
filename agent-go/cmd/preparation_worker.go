package cmd

import (
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	"github.com/spf13/cobra"
)

func init() {
	var stateDir, workID string
	worker := &cobra.Command{Use: "preparation-worker", Hidden: true, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return executor.RunPreparationWorker(stateDir, workID) }}
	worker.Flags().StringVar(&stateDir, "state-dir", "", "Private agent state directory")
	worker.Flags().StringVar(&workID, "work-id", "", "Exact preparation work identity")
	rootCmd.AddCommand(worker)
}
