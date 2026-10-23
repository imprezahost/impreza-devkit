package executor

import (
	"context"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func (d *Docker) onionPublicationNote(ctx context.Context, cmd *sdkclient.PollCommand, deploymentID, onion string) string {
	if onion == "" {
		return ""
	}
	d.deploymentProgress(ctx, cmd, "checking_readiness")
	if d.Log != nil {
		d.Log.Info("publishing .onion")
	}
	start := time.Now()
	wait := d.onionPublicationWait
	if wait == nil && d.Tor != nil {
		wait = d.Tor.WaitForOnionPublication
	}
	if wait != nil && wait(ctx, deploymentID, onion) && ctx.Err() == nil {
		if d.Log != nil {
			d.Log.Info("onion descriptor upload observed", "elapsed_ms", time.Since(start).Milliseconds())
		}
		return "\nonion descriptor upload observed by service Tor (daemon event, not reachability or application health)"
	}
	return "\npublishing .onion (advisory): descriptor upload by service Tor is not confirmed"
}
