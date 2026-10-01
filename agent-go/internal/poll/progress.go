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
	if p.active.DomainHandover != nil {
		d, ok := p.exec.(*executor.Docker)
		if !ok {
			return errors.New("domain handover recovery requires Docker executor")
		}
		result, err := d.RecoverDomainHandover(ctx, p.active.CommandID, p.active.DomainHandover)
		if err != nil {
			return err
		}
		result.ControlToken = p.active.ControlToken
		p.active.Result = &result
		if err := p.journal.save(p.active); err != nil {
			return err
		}
		return p.sendSavedResult(ctx)
	}
	if p.active.Kind == sdkclient.CommandHostFailoverFence {
		// A fence writes its persistent tombstone before touching routes or
		// containers. Re-executing the SAME reviewed command after a crash is
		// safe; the executor refuses an older or different epoch. Never turn
		// this into a generic deploy preparation recovery.
		if !validFencePayload(p.active.Payload) {
			return errors.New("saved host failover fence cannot be verified")
		}
		cmd := &sdkclient.PollCommand{ID: p.active.CommandID, Kind: p.active.Kind,
			ControlToken: p.active.ControlToken, ProgressProtocol: p.active.ProgressProtocol,
			Payload: p.active.Payload}
		result := p.exec.Execute(ctx, cmd)
		if result.CommandID != cmd.ID {
			return errors.New("fence executor returned a result for another command")
		}
		result.ControlToken = cmd.ControlToken
		p.active.Result = &result
		if err := p.journal.save(p.active); err != nil {
			return fmt.Errorf("persist recovered fence result: %w", err)
		}
		return p.sendSavedResult(ctx)
	}

	if p.active.Replacement != nil {
		return p.resumeReplacement(ctx)
	}
	if p.active.Kind == sdkclient.CommandDeploy {
		// A restore-quiesce job owns its own durable journal; the phases
		// resume from it, and the recorded outcome answers a resend.
		if docker, ok := p.exec.(*executor.Docker); ok {
			command := &sdkclient.PollCommand{ID: p.active.CommandID, ControlToken: p.active.ControlToken, ProgressProtocol: p.active.ProgressProtocol}
			for ctx.Err() == nil {
				result, found := docker.RecoverRestoreQuiesce(ctx, command)
				if !found {
					break
				}
				if result.Status == executor.PreparationPendingStatus {
					// The drive ran out of context (agent shutdown); the
					// journal keeps the operation and the next boot retries.
					if !sleepCtx(ctx, 15*time.Second) {
						return nil
					}
					continue
				}
				result.ControlToken = p.active.ControlToken
				next := *p.active
				next.Result = &result
				if err := p.journal.save(&next); err != nil {
					return fmt.Errorf("persist recovered restore result: %w", err)
				}
				p.active = &next
				return p.sendSavedResult(ctx)
			}
		}
	}
	p.log.Error("interrupted operation has no saved result; execution will not be repeated", "command_id", p.active.CommandID)
	for ctx.Err() == nil {
		if r := p.active.Preparation; r != nil && r.Phase == "busy" && r.Work != nil {
			docker, ok := p.exec.(*executor.Docker)
			if !ok {
				return errors.New("supervised preparation requires Docker executor")
			}
			command := &sdkclient.PollCommand{ID: p.active.CommandID, ControlToken: p.active.ControlToken}
			if _, controlErr := docker.InterruptPreparationWork(ctx, command, r.Work, r.DeploymentID); controlErr != nil {
				p.log.Warn("active build interruption not confirmed", "command_id", p.active.CommandID, "err", controlErr)
			}
			if _, recoveryErr := docker.RecoverOwnedPreparation(ctx, command, r.Work, r.DeploymentID); recoveryErr != nil {
				p.log.Warn("owned build recovery not confirmed", "command_id", p.active.CommandID, "err", recoveryErr)
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
				response, reportErr := p.reportProgress(ctx, step)
				// The server already closed the command and nothing of the work
				// can still change images — its unit is confirmed stopped, and it
				// is not a controlled build, whose builder container can outlive
				// the unit. Waiting would hold every later command on this host —
				// uninstalls, onion revocations, updates — forever.
				if reportErr == nil && response.Terminal && docker.PreparationWorkerGone(r) {
					if released, releaseErr := p.releaseTerminalPreparation(ctx, docker); releaseErr != nil || released {
						return releaseErr
					}
				}
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
		if err == nil && response.Terminal {
			// The server already closed the command, and nothing below reached
			// a worker that may still run (the busy branch above waits for its
			// unit to stop). A Blocked deployment clears like the legacy
			// "blocked" phase did; an aborted checkpoint used to wait here for
			// a manual reconciliation that nobody runs, holding the queue
			// forever. The operation is never replayed either way.
			docker, _ := p.exec.(*executor.Docker)
			if released, releaseErr := p.releaseTerminalPreparation(ctx, docker); releaseErr != nil || released {
				return releaseErr
			}
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
			if p.active.Kind == sdkclient.CommandAgentUpgrade && p.active.Result.Status == "success" {
				docker, ok := p.exec.(*executor.Docker)
				if !ok {
					return errors.New("managed update requires Docker executor")
				}
				if err := docker.StartAgentUpgrade(p.active.CommandID); err != nil {
					return fmt.Errorf("start acknowledged managed agent update: %w", err)
				}
			}
			if p.active.DomainHandover != nil && p.active.Result.DomainHandover != nil && p.active.Result.DomainHandover.Status != "recovery_required" {
				if d, ok := p.exec.(*executor.Docker); ok {
					if err := d.ForgetDomainHandover(p.active.CommandID); err != nil {
						return err
					}
				}
			}
			p.forgetPreparationWork()
			p.forgetReplacementWork()
			p.forgetRestoreQuiesce()
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
		released, err := p.releaseTerminalPreparation(ctx, docker)
		if err != nil || released {
			return err
		}
		return errors.New("the server closed the command, and the previous configuration is not verified and restored yet; the journal stays for review")
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
		result.Error = "Preparation was interrupted without a successful completion receipt. The prior container identities were verified and configuration restored. No build or deployment was replayed; retry explicitly when ready."
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

// releaseTerminalPreparation clears the journal of a command the server
// already closed, once no preparation worker can still be changing images,
// and reports whether it did. A recoverable checkpoint is released only after
// its previous configuration is restored — verified locally against the
// recorded containers and file bytes, which needs no server confirmation
// because there is no result left to send. Without that restore the next
// command would run the failed deployment's files, so the journal stays and
// the restore is tried again (a changed container set really is a review
// case). A checkpoint that never reconciles (Blocked, unstarted, replacing)
// clears as it always did. Waiting for a manual reconciliation nobody runs
// used to hold every later command on the host forever. Worker files go
// first: request.json can carry proxy credentials, and nothing else would
// ever remove them.
func (p *Poller) releaseTerminalPreparation(ctx context.Context, docker *executor.Docker) (bool, error) {
	if r := p.active.Preparation; r != nil {
		checkpoint := *r
		if checkpoint.Phase == "busy" {
			checkpoint.Phase = "aborted"
		}
		if checkpoint.Recoverable() && checkpoint.Phase != "unstarted" {
			if docker == nil {
				return false, nil
			}
			if err := docker.ReconcilePreparation(ctx, &checkpoint); err != nil {
				p.log.Warn("the server closed the command; its previous configuration is not verified and restored yet",
					"command_id", p.active.CommandID, "err", err)
				return false, nil
			}
			p.log.Warn("the server closed the command; previous configuration restored and journal released",
				"command_id", p.active.CommandID)
		}
	}
	p.forgetPreparationWork()
	if err := p.journal.clear(); err != nil {
		return false, err
	}
	p.active = nil
	return true, nil
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

// forgetRestoreQuiesce drops the restore journal of an acknowledged
// command, undo area included: the outcome is recorded where the customer
// reads it by then.
func (p *Poller) forgetRestoreQuiesce() {
	if docker, ok := p.exec.(*executor.Docker); ok {
		if err := docker.ForgetRestoreQuiesce(p.active.CommandID); err != nil {
			p.log.Warn("acknowledged restore quiesce journal retained", "command_id", p.active.CommandID, "err", err)
		}
	}
}

// ActiveCommandID names the operation the saved journal owns, if any, so
// startup reconciliation can leave it to resumeRecord.
func (p *Poller) ActiveCommandID() string {
	if p.active == nil {
		return ""
	}
	return p.active.CommandID
}
