package executor

// Recovery side of restore-quiesce-v1. The live run and the recovery after
// an agent restart share quiesceDrive; this file owns the durable result
// receipt, the command-to-journal lookup, and the startup reconciliation
// that never lets a quiesced application stay down without an outcome.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

type restoreQuiesceResultFile struct {
	Version   int                    `json:"version"`
	CommandID string                 `json:"command_id"`
	Result    sdkclient.DeployResult `json:"result"`
}

// quiesceWriteResult records the terminal outcome of a quiesce restore
// before it is reported, so a crash between finishing and acknowledging
// still has an answer for a resent command.
func (d *Docker) quiesceWriteResult(jobID string, result sdkclient.DeployResult) {
	dir, err := d.quiesceDir(jobID)
	if err != nil {
		return
	}
	raw, err := json.Marshal(restoreQuiesceResultFile{Version: 1, CommandID: result.CommandID, Result: result})
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	if err := writeAtomic(filepath.Join(dir, "result.json"), raw, quiesceJournalFileMode); err != nil {
		d.Log.Warn("restore quiesce: terminal result could not be journaled", "err", err)
	}
}

// quiesceReadResult returns the recorded terminal outcome for a command.
func (d *Docker) quiesceReadResult(jobID, commandID string) (sdkclient.DeployResult, bool) {
	dir, err := d.quiesceDir(jobID)
	if err != nil {
		return sdkclient.DeployResult{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return sdkclient.DeployResult{}, false
	}
	var file restoreQuiesceResultFile
	if json.Unmarshal(raw, &file) != nil || file.Version != 1 || file.CommandID != commandID ||
		file.Result.CommandID != commandID || file.Result.DeploymentID != jobID {
		return sdkclient.DeployResult{}, false
	}
	return file.Result, true
}

// quiesceJournals lists the restore journal directories on this agent.
func (d *Docker) quiesceJournals() ([]string, error) {
	base := filepath.Join(d.StateDir, "operations")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 1024 {
		return nil, errors.New("too many operation journals")
	}
	var jobs []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "restore-") {
			continue
		}
		jobID := strings.TrimPrefix(entry.Name(), "restore-")
		if !quiesceJobID.MatchString(jobID) {
			return nil, errors.New("unrecognized restore journal directory")
		}
		jobs = append(jobs, jobID)
	}
	return jobs, nil
}

// quiesceJobByCommand finds the journal a command owns, through either its
// live record or its recorded result.
func (d *Docker) quiesceJobByCommand(commandID string) (string, error) {
	jobs, err := d.quiesceJournals()
	if err != nil {
		return "", err
	}
	for _, jobID := range jobs {
		if record, err := d.loadQuiesceRecord(jobID); err == nil && record != nil && record.CommandID == commandID {
			return jobID, nil
		}
		if _, ok := d.quiesceReadResult(jobID, commandID); ok {
			return jobID, nil
		}
	}
	return "", nil
}

// RecoverRestoreQuiesce resumes the quiesce restore a saved command owns:
// it answers from the recorded result when the work already finished, and
// otherwise drives the phases from the journal. found=false means the
// command owns no quiesce journal and recovery does not apply.
func (d *Docker) RecoverRestoreQuiesce(ctx context.Context, cmd *sdkclient.PollCommand) (result sdkclient.DeployResult, found bool) {
	jobID, err := d.quiesceJobByCommand(cmd.ID)
	if err != nil {
		return failResult(cmd.ID, "restore quiesce journal cannot be verified: "+err.Error()), true
	}
	if jobID == "" {
		return sdkclient.DeployResult{}, false
	}
	if recorded, ok := d.quiesceReadResult(jobID, cmd.ID); ok {
		return recorded, true
	}
	return d.quiesceDrive(ctx, cmd, jobID), true
}

// ReconcileRestoreQuiesce runs under the agent's operation lock at
// startup. The journal the poller still owns is left to resumeRecord; any
// other restore journal is finished here — an outcome already recorded is
// released, a live one is driven to its end — because the quiesced
// application must never stay down waiting for a command that will never
// come. It blocks: correctness of the customer's data outranks the poll
// loop's availability, the same posture as the failover fences.
//
// A journal that cannot be verified no longer takes the whole agent down:
// the deployment it names (or every data-touching operation, when even the
// target is unreadable) is poisoned — refused with the reason, visible to
// the control plane in every refused result — while the agent keeps
// serving everything else. Manual reconciliation of the journal directory
// clears the condition at the next restart.
func (d *Docker) ReconcileRestoreQuiesce(ctx context.Context, activeCommandID string) error {
	jobs, err := d.quiesceJournals()
	if err != nil {
		return err
	}
	for _, jobID := range jobs {
		record, err := d.loadQuiesceRecord(jobID)
		if err != nil {
			d.poisonQuiesce(jobID, err)
			continue
		}
		if record != nil && record.CommandID == activeCommandID {
			// The saved operation owns this journal; resumeRecord drives it.
			continue
		}
		dir, err := d.quiesceDir(jobID)
		if err != nil {
			return err
		}
		if record == nil {
			// No live record: either only the terminal result remains (its
			// command was acknowledged or belongs to no one) or a leftover
			// directory. Nothing here can still be holding the app down.
			raw, readErr := os.ReadFile(filepath.Join(dir, "result.json"))
			if readErr == nil {
				var file restoreQuiesceResultFile
				if json.Unmarshal(raw, &file) == nil && file.Version == 1 {
					d.Log.Warn("restore quiesce: releasing an unowned finished restore journal", "job", jobID, "status", file.Result.Status)
				}
			}
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			continue
		}
		if record.Phase == "quiesce_orphan" {
			// The job survived the teardown. No automatic path
			// starts the application from this state — the journal is
			// terminal and stays for support to review. If the job has
			// since died on its own, release the journal (the result is
			// already recorded); otherwise keep it quarantined and
			// poison the target so no new restore (or any data-touching
			// operation) runs on data a live job is writing into.
			if d.quiesceJobStillRunning(ctx, jobID) {
				d.Log.Error("restore quiesce: orphaned journal with a live job; the application stays stopped and the target is quarantined until support acts", "job", jobID)
				if d.quiescePoisoned == nil {
					d.quiescePoisoned = map[string]string{}
				}
				if record.Target != "" {
					d.quiescePoisoned[record.Target] = fmt.Sprintf("restore journal %s is quarantined (quiesce_orphan) with a live job; the application stays stopped until support confirms the job is dead", jobID)
				} else {
					d.quiescePoisoned[""] = fmt.Sprintf("restore journal %s is quarantined (quiesce_orphan) with a live job; every data-changing operation is refused until support confirms the job is dead", jobID)
				}
				continue
			}
			d.Log.Warn("restore quiesce: orphaned journal with the job now dead; releasing for support review", "job", jobID)
			if err := d.forgetQuiesceRecord(jobID); err != nil {
				return err
			}
			continue
		}
		d.Log.Warn("restore quiesce: finishing an unowned interrupted restore", "job", jobID, "phase", record.Phase)
		// Safety: before driving any journal to completion — which
		// starts the application — confirm no container of the job
		// project is still alive. An inspection error counts as alive.
		if d.quiesceJobStillRunning(ctx, jobID) {
			d.Log.Error("restore quiesce: unowned journal has a live job; refusing to start the application", "job", jobID)
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, quiesceReconcileBudget())
		result := d.quiesceDrive(bounded, &sdkclient.PollCommand{ID: record.CommandID}, jobID)
		cancel()
		if result.Status == PreparationPendingStatus {
			return fmt.Errorf("unowned restore quiesce journal %s did not finish within the reconciliation budget", jobID)
		}
		if result.Status != "success" {
			d.Log.Error("restore quiesce: unowned restore finished failed; the outcome is on disk for review", "job", jobID, "error", result.Error)
		}
		if err := d.forgetQuiesceRecord(jobID); err != nil {
			return err
		}
	}
	return nil
}

// quiesceReconcileBudget bounds the startup drive of an unowned journal.
// The drive honours the deadlines the journal pinned, so it ends by
// itself within the two ceilings plus the abort wait; the budget only
// guards against a hang and must never be shorter than that, or the
// startup fails, the agent restarts and drives the same journal again —
// the 45 min the ceilings of 2 h used to outlast.
func quiesceReconcileBudget() time.Duration {
	return quiesceReadyCeiling + quiesceExitCeiling + quiesceAbortWait + 15*time.Minute
}

// poisonQuiesce quarantines what an unverifiable journal puts at risk.
// The target comes only from the terminal result's receipt — anything
// else could poison the wrong application and leave the real one, with
// its data mid-exchange, free to be touched. Without a readable receipt
// the target is unknown and every data-touching operation is refused
// until someone reconciles the journal by hand.
func (d *Docker) poisonQuiesce(jobID string, cause error) {
	target, known := d.quiescePoisonTarget(jobID)
	reason := fmt.Sprintf("restore quiesce journal %s cannot be verified (%v); manual reconciliation required", jobID, cause)
	if !known {
		target = ""
		reason = fmt.Sprintf("restore quiesce journal %s cannot be verified and names no application (%v); every data-changing operation is refused until support reconciles it", jobID, cause)
	}
	if d.quiescePoisoned == nil {
		d.quiescePoisoned = map[string]string{}
	}
	d.quiescePoisoned[target] = reason
	d.Log.Error("restore quiesce: journal is corrupt; failing closed for the affected application until reconciled",
		"job", jobID, "target", target, "cause", cause)
}

// quiescePoisonTarget recovers the application a corrupt journal belongs
// to. Only the terminal result file's receipt names the target: guessing
// from staging directories poisoned the wrong application (an old
// .restore-* of another app), leaving the real one — mid-exchange — free
// to be touched. Without a readable result, every data-touching operation
// is refused until support reconciles the journal.
func (d *Docker) quiescePoisonTarget(jobID string) (string, bool) {
	dir, err := d.quiesceDir(jobID)
	if err != nil {
		return "", false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return "", false
	}
	var file restoreQuiesceResultFile
	if json.Unmarshal(raw, &file) != nil || file.Version != 1 || file.Result.RestoreQuiesce == nil {
		return "", false
	}
	// The receipt must be THIS journal's: a result.json another job left
	// (copied or moved in) named another application, poisoned it, and
	// left the real target — mid-exchange — free to be touched.
	if file.Result.DeploymentID != jobID || file.CommandID == "" || file.Result.CommandID != file.CommandID {
		return "", false
	}
	target := file.Result.RestoreQuiesce.Target
	if !failoverDeploymentID.MatchString(target) {
		return "", false
	}
	return target, true
}

// quarantineScope says which deployment of a command a quarantined
// (poisoned) restore journal must protect.
type quarantineScope int

const (
	// quarantineAllowed: the command cannot change the application's data
	// or bring anything back onto it, so support keeps it while the journal
	// is reconciled. health_check and logs_tail read; host_failover_fence
	// only stops the containers, removes the routes and withdraws the onion
	// (it moves risk away); host_failover_release lifts a fence but keeps
	// the containers stopped; onion_key_export seals the current identity
	// for a standby, the way out of a quarantined host; routes, ingress and
	// the onion profile/auth only shape access to an application that stays
	// stopped.
	quarantineAllowed quarantineScope = iota
	// quarantineBoth: refused when the deployment itself or the target it
	// names is quarantined. These change the application's data or
	// containers (deploy, rollback, uninstall, restart), or irreversibly
	// change the identity support is reconciling (onion_rotate parks the
	// live key and publishes a new address; onion_purge destroys parked
	// recovery copies), or make the quarantined application the receiver of
	// a failover (onion_transfer_recipient), which would put visitors on
	// data nobody verified.
	quarantineBoth
	// quarantineTarget: traffic_switch moves a hostname FROM the deployment
	// TO a target. Moving visitors onto a quarantined target is refused;
	// moving them off a quarantined source is how a customer gets back
	// online, so the source alone does not refuse it.
	quarantineTarget
)

func quarantineScopeOf(kind sdkclient.CommandKind) quarantineScope {
	switch kind {
	case sdkclient.CommandDeploy, sdkclient.CommandRollback, sdkclient.CommandUninstall,
		sdkclient.CommandRestart, sdkclient.CommandOnionRotate, sdkclient.CommandOnionPurge,
		sdkclient.CommandOnionTransferRecipient:
		return quarantineBoth
	case sdkclient.CommandTrafficSwitch:
		return quarantineTarget
	default:
		return quarantineAllowed
	}
}

// quiesceRefusal reports why a command may not touch the deployment, when
// a poisoned journal says it may not. An unknown target ("") refuses every
// command that is not quarantineAllowed.
func (d *Docker) quiesceRefusal(kind sdkclient.CommandKind, deploymentID, target string) string {
	if d.quiescePoisoned == nil {
		return ""
	}
	scope := quarantineScopeOf(kind)
	if scope == quarantineAllowed {
		return ""
	}
	if reason, poisoned := d.quiescePoisoned[""]; poisoned {
		return reason
	}
	if scope == quarantineBoth {
		if reason, poisoned := d.quiescePoisoned[deploymentID]; poisoned && deploymentID != "" {
			return reason
		}
	}
	if reason, poisoned := d.quiescePoisoned[target]; poisoned && target != "" {
		return reason
	}
	return ""
}

// quiesceExecuteRefusal is the Execute-level arm of the poison: it reads
// the target a command names (a restore job's IMPREZA_TARGET, a traffic
// switch's target_deployment_id) and refuses when the poisoned journal is
// the application's.
func (d *Docker) quiesceExecuteRefusal(cmd *sdkclient.PollCommand, deploymentID string) string {
	target := ""
	switch cmd.Kind {
	case sdkclient.CommandDeploy:
		var payload struct {
			Vars map[string]any `json:"vars"`
		}
		if cmd.As(&payload) == nil {
			target, _ = payload.Vars["IMPREZA_TARGET"].(string)
		}
	case sdkclient.CommandTrafficSwitch:
		var payload struct {
			TargetDeploymentID string `json:"target_deployment_id"`
		}
		if cmd.As(&payload) == nil {
			target = payload.TargetDeploymentID
		}
	}
	return d.quiesceRefusal(cmd.Kind, deploymentID, target)
}

// maxQuarantineReports bounds the heartbeat field: one entry per poisoned
// journal target, never more than an agent can plausibly hold.
const maxQuarantineReports = 32

// CollectRestoreQuarantine is the heartbeat's view of the poison:
// without it the control plane only learned of a quarantined application
// from the result of a refused operation, and an application stopped by a
// restore could sit down with nobody told. The map is written only by the
// startup reconciliation, before the heartbeat loop starts, so reading it
// here races with nothing. Ordered and bounded; the reason is the same text
// a refused result already carries (the restore job id and why the journal
// is held), never a secret.
func (d *Docker) CollectRestoreQuarantine() []sdkclient.RestoreQuarantineReport {
	if len(d.quiescePoisoned) == 0 {
		return nil
	}
	keys := make([]string, 0, len(d.quiescePoisoned))
	for k := range d.quiescePoisoned {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]sdkclient.RestoreQuarantineReport, 0, len(keys))
	for _, k := range keys {
		if len(out) == maxQuarantineReports {
			break
		}
		r := sdkclient.RestoreQuarantineReport{DeploymentID: k, Scope: "application", Reason: d.quiescePoisoned[k]}
		if k == "" {
			r.Scope = "all"
		}
		if len(r.Reason) > 512 {
			r.Reason = strings.ToValidUTF8(r.Reason[:512], "")
		}
		out = append(out, r)
	}
	return out
}
