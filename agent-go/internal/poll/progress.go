package poll

import (
	"context"
	"errors"
	"fmt"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
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
		if p.active.Preparation.Recoverable() {
			if docker, ok := p.exec.(*executor.Docker); ok {
				if err := p.reconcilePreparation(ctx, docker); err == nil {
					return nil
				} else {
					p.log.Warn("automatic preparation reconciliation not confirmed", "command_id", p.active.CommandID, "err", err)
				}
			}
		}
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

func (p *Poller) savePreparation(cmd *sdkclient.PollCommand, state *executor.PreparationRecovery) error {
	if p.journalErr != nil {
		return p.journalErr
	}
	if p.active == nil || p.active.CommandID != cmd.ID || p.active.ControlToken != cmd.ControlToken {
		p.journalErr = errors.New("preparation checkpoint belongs to another operation")
		return p.journalErr
	}
	if err := state.Validate(); err != nil {
		p.journalErr = err
		return err
	}
	next := *p.active
	copyState := *state
	next.Preparation = &copyState
	if err := p.journal.save(&next); err != nil {
		p.journalErr = err
		return err
	}
	p.active = &next
	return nil
}
func (p *Poller) reconcilePreparation(ctx context.Context, docker *executor.Docker) error {
	response, err := p.reportProgress(ctx, "interrupted")
	if err != nil {
		return err
	}
	if response.Terminal {
		if err := p.journal.clear(); err != nil {
			return err
		}
		p.active = nil
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	control, err := p.client.AgentCommandControl(checkCtx, sdkclient.DeploymentControl{CommandID: p.active.CommandID, ControlToken: p.active.ControlToken, Phase: "preparing"})
	if err != nil {
		return err
	}
	if control.CommandID != p.active.CommandID || control.Phase != "preparing" {
		return errors.New("server did not confirm preparation identity and phase")
	}
	// Older control planes may not know this display step. Failure to display it
	// does not bypass the authenticated phase check or local recovery validation.
	_, _ = p.reportProgress(ctx, "reconciling_preparation")
	if err := docker.ReconcilePreparation(ctx, p.active.Preparation); err != nil {
		return err
	}
	result := sdkclient.DeployResult{CommandID: p.active.CommandID, ControlToken: p.active.ControlToken, DeploymentID: p.active.Preparation.DeploymentID, Status: "failed", PreparationRestored: true, Error: "Agent interrupted before container replacement. The completed preparation checkpoint was verified and previous configuration restored. Deployment was not repeated; retry explicitly when ready."}
	if control.CancelRequested {
		result.Status = "cancelled"
		result.Error = "Requested cancellation confirmed after interrupted preparation was reconciled. Deployment was not repeated."
	}
	if p.active.Preparation.Phase == "unstarted" {
		result.Error = "Agent interrupted before deployment execution started. No deployment work was repeated."
	}
	next := *p.active
	next.Result = &result
	if err := p.journal.save(&next); err != nil {
		return err
	}
	p.active = &next
	return p.sendSavedResult(ctx)
}
