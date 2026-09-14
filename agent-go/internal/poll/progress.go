package poll

import (
	"context"
	"fmt"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"time"
)

func (p *Poller) observeProgress(ctx context.Context, cmd *sdkclient.PollCommand, step string) {
	if p.active == nil || p.active.CommandID != cmd.ID {
		return
	}
	p.active.Step = step
	if err := p.journal.save(p.active); err != nil {
		p.log.Warn("could not persist current operation step", "command_id", cmd.ID, "err", err)
	}
	_, _ = p.reportProgress(ctx, step)
}
func (p *Poller) reportProgress(ctx context.Context, step string) (*sdkclient.DeploymentProgressResponse, error) {
	reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := p.client.AgentCommandProgress(reportCtx, sdkclient.DeploymentProgress{CommandID: p.active.CommandID, ControlToken: p.active.ControlToken, Step: step})
	if err == nil && response.CommandID != p.active.CommandID {
		err = fmt.Errorf("progress response belongs to another operation")
	}
	if err != nil {
		p.log.Warn("operation progress could not be reported", "command_id", p.active.CommandID, "err", err)
	}
	return response, err
}
func (p *Poller) resumeRecord(ctx context.Context) error {
	if p.active.Result != nil {
		_, _ = p.reportProgress(ctx, "resending_result")
		return p.sendSavedResult(ctx)
	}
	p.log.Error("interrupted operation has no saved result; execution will not be repeated", "command_id", p.active.CommandID)
	for ctx.Err() == nil {
		response, err := p.reportProgress(ctx, "interrupted")
		if err == nil && response.Terminal {
			if err := p.journal.clear(); err != nil {
				return err
			}
			p.active = nil
			return nil
		}
		if !sleepCtx(ctx, 15*time.Second) {
			break
		}
	}
	return nil
}
func (p *Poller) sendSavedResult(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := p.client.AgentDeployResult(ctx, *p.active.Result); err == nil {
			if err := p.journal.clear(); err != nil {
				return fmt.Errorf("remove acknowledged receipt: %w", err)
			}
			p.active = nil
			return nil
		} else {
			p.log.Warn("saved deployment result awaiting acknowledgement", "command_id", p.active.CommandID, "err", err)
		}
		if !sleepCtx(ctx, 5*time.Second) {
			break
		}
	}
	return nil
}
