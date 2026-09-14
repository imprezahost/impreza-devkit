package executor

import (
	"context"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func (d *Docker) deploymentProgress(ctx context.Context, cmd *sdkclient.PollCommand, step string) {
	if cmd.ProgressProtocol == sdkclient.DeploymentProgressProtocol && d.Progress != nil {
		d.Progress(ctx, cmd, step)
	}
}
