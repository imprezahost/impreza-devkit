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

	if p.active.Replacement != nil {
		return p.resumeReplacement(ctx)
	}
	p.log.Error("interrupted operation has no saved result; execution will not be repeated", "command_id", p.active.CommandID)
	for ctx.Err() == nil {
		if r := p.active.Preparation; r != nil && r.Phase == "busy" && r.Work != nil {
			docker, ok := p.exec.(*executor.Docker)
			if !ok {
				return errors.New("supervised preparation requires Docker executor")
			}
			ready, err := docker.CompletedPreparationWork(r, p.active.CommandID)
			if err == nil {
				command := &sdkclient.PollCommand{ID: p.active.CommandID, ControlToken: p.active.ControlToken}
				if err = p.savePreparation(command, ready); err != nil {
					return err
				}
			} else {
				// Even a server-side terminal state cannot authorize a new local command
				// while this worker might still be changing images. Never relaunch it.
				step := "interrupted"
				if errors.Is(err, executor.ErrPreparationPending) {
					step = "reconciling_preparation"
				}
				_, _ = p.reportProgress(ctx, step)
				p.log.Warn("supervised preparation awaiting verified completion", "command_id", p.active.CommandID, "err", err)
				if !sleepCtx(ctx, 15*time.Second) {
					break
				}
				continue
			}
		}
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
		if err == nil && response.Terminal && (p.active.Preparation == nil || p.active.Preparation.Phase != "aborted") {
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
			p.forgetPreparationWork()
			p.forgetReplacementWork()
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
		if p.active.Preparation != nil && p.active.Preparation.Phase == "aborted" {
			return errors.New("reboot recovery requires an authenticated preparing phase; terminal server state requires manual reconciliation")
		}
		p.forgetPreparationWork()
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
	if p.active.Preparation.Phase == "aborted" {
		result.Error = "Host reboot interrupted preparation without a completion receipt. The prior container identities were verified and configuration restored. No build or deployment was replayed; retry explicitly when ready."
	}
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

func (p *Poller) forgetPreparationWork() {
	if p.active.Preparation != nil && p.active.Preparation.Work != nil {
		if docker, ok := p.exec.(*executor.Docker); ok {
			if err := docker.ForgetPreparationWork(p.active.Preparation.Work); err != nil {
				p.log.Warn("completed preparation files retained", "err", err)
			}
		}
	}
}
