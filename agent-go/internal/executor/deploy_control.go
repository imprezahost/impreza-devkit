package executor

import (
	"context"
	"errors"
	"fmt"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"time"
)

var errDeployCancelled = errors.New("deployment cancellation requested before container replacement")

// Checkpoints run between completed operations. In particular, an image pull or
// build finishes before cancellation is acknowledged: no detached Docker build
// is treated as stopped merely because its client process was killed.
func (d *Docker) deploymentCheckpoint(ctx context.Context, cmd *sdkclient.PollCommand, phase string) error {
	if cmd.ControlToken == "" {
		return nil
	}
	if d.Client == nil {
		return errors.New("deployment control client unavailable")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	answer, err := d.Client.AgentCommandControl(checkCtx, sdkclient.DeploymentControl{CommandID: cmd.ID, ControlToken: cmd.ControlToken, Phase: phase})
	if err != nil {
		return fmt.Errorf("deployment checkpoint could not be confirmed: %w", err)
	}
	if answer.CommandID != cmd.ID {
		return errors.New("deployment checkpoint returned a different operation")
	}
	if answer.CancelRequested {
		return errDeployCancelled
	}
	if answer.Phase != phase {
		return errors.New("deployment checkpoint returned an unexpected phase")
	}
	return nil
}
func preparationResult(commandID string, err error) sdkclient.DeployResult {
	result := failResult(commandID, err.Error())
	if errors.Is(err, ErrPreparationPending) {
		result.Status = PreparationPendingStatus
		return result
	}
	if errors.Is(err, errDeployCancelled) {
		result.Status = "cancelled"
		result.PreparationRestored = true
	}
	return result
}
