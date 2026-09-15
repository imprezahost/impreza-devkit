package poll

import (
	"context"
	"errors"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"time"
)

func validateReplacementRecord(record *commandRecord) error {
	w := record.Replacement
	if w == nil {
		return nil
	}
	if err := w.Validate(); err != nil {
		return err
	}
	if w.CommandID != record.CommandID || record.Preparation == nil || record.Preparation.Phase != "replacing" || record.Preparation.Work != nil || record.Preparation.DeploymentID != w.DeploymentID {
		return errors.New("replacement worker does not match authorized operation")
	}
	return nil
}
func (p *Poller) saveReplacement(cmd *sdkclient.PollCommand, w *executor.ReplacementWork) error {
	if p.journalErr != nil {
		return p.journalErr
	}
	if p.active == nil || p.active.CommandID != cmd.ID || p.active.ControlToken != cmd.ControlToken || p.active.Replacement != nil {
		p.journalErr = errors.New("replacement identity cannot be assigned to this operation")
		return p.journalErr
	}
	next := *p.active
	copyWork := *w
	next.Replacement = &copyWork
	if err := validateReplacementRecord(&next); err != nil {
		p.journalErr = err
		return err
	}
	if err := p.journal.save(&next); err != nil {
		p.journalErr = err
		return err
	}
	p.active = &next
	return nil
}
func (p *Poller) resumeReplacement(ctx context.Context) error {
	docker, ok := p.exec.(*executor.Docker)
	if !ok {
		return errors.New("supervised replacement requires Docker executor")
	}
	if err := validateReplacementRecord(p.active); err != nil {
		return err
	}
	for ctx.Err() == nil {
		result, err := docker.CompletedReplacementWork(p.active.Replacement)
		if err == nil {
			result.ControlToken = p.active.ControlToken
			next := *p.active
			next.Result = result
			if err = p.journal.save(&next); err != nil {
				return err
			}
			p.active = &next
			_, _ = p.reportProgress(ctx, "resending_result")
			return p.sendSavedResult(ctx)
		}
		step := "interrupted"
		if errors.Is(err, executor.ErrReplacementPending) {
			step = "reconciling_replacement"
		}
		_, _ = p.reportProgress(ctx, step)
		p.log.Warn("supervised replacement awaiting verified completion", "command_id", p.active.CommandID, "err", err)
		// Even a terminal server status cannot release a local worker that might
		// still be changing containers, routes or running customer lifecycle hooks.
		if !sleepCtx(ctx, 15*time.Second) {
			break
		}
	}
	return nil
}
func (p *Poller) forgetReplacementWork() {
	if p.active.Replacement != nil {
		if docker, ok := p.exec.(*executor.Docker); ok {
			if err := docker.ForgetReplacementWork(p.active.Replacement); err != nil {
				p.log.Warn("completed replacement files retained", "err", err)
			}
		}
	}
}
