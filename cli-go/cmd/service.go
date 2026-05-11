package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Service lifecycle verbs (cancel for now; more in future fases).",
	Long: `Service termination is staff-owned by design: the customer submits a
cancellation request via this verb, the team approves the actual
termination. There is no direct customer path to terminate a service.`,
}

var (
	svcCancelType   string
	svcCancelReason string
)

var serviceCancelCmd = &cobra.Command{
	Use:   "cancel <service-id>",
	Short: "Submit a cancellation request for any service (VPS, hosting, email, domain).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("service-id must be an integer: %s", args[0])
		}
		if err := confirmOrExit(cmd.InOrStdin(), cmd.ErrOrStderr(),
			fmt.Sprintf("Submit %q cancel request for service %d?", svcCancelType, id),
			autoYes); err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		if err := c.ServiceCancel(cmd.Context(), id, svcCancelType, svcCancelReason); err != nil {
			return err
		}
		output.Success("Cancel request submitted for service %d (type=%s). Staff will approve the termination.",
			id, svcCancelType)
		return nil
	},
}

func init() {
	serviceCancelCmd.Flags().StringVar(&svcCancelType, "type", "End of Billing Period",
		"Cancel type: 'Immediate' or 'End of Billing Period'.")
	serviceCancelCmd.Flags().StringVar(&svcCancelReason, "reason", "",
		"Optional reason text the cancellation request carries.")
	serviceCancelCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Skip the confirmation prompt.")

	serviceCmd.AddCommand(serviceCancelCmd)
	rootCmd.AddCommand(serviceCmd)
}
