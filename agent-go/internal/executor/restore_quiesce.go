package executor

// restore-quiesce-v1: a file-restore transport job runs with the
// target application stopped. The running database is what loses the last
// writes before a backup when a restore swaps the data tree underneath it
// and only restarts afterwards: the shutdown checkpoint lands on the
// restored files and the WAL replay never sees the transactions the
// archive still held. One command therefore owns the whole sequence —
// stage with the app running, stop and verify, release the swap, wait for
// the job's real exit code, start the app again, and undo the swap when
// the job failed after data moved — with a journal on disk so an agent
// restart in the middle resumes instead of leaving the app down.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const (
	// quiesceExitSwapped is the restore script's exit code for "the swap
	// had started and failed after data moved". Contract with the control
	// plane's job script; every other non-zero code means "nothing was
	// changed". The agent additionally infers the swap from the displaced
	// tree on disk, so an older script cannot strand a half-restored app.
	quiesceExitSwapped = 75
	// quiesceStopDefault bounds `docker stop` per container when the
	// payload does not say.
	quiesceStopDefault = 60

	// quiescePollInterval is the handshake sampling period. Files and
	// `docker inspect` are cheap; a restore window of minutes does not
	// need a faster loop than this.
	quiescePollInterval = time.Second
)

// The journal carries the operation's identities and outcome; the fence
// journals' private modes are the policy here too. Variables so a test
// pins them on every platform (Windows cannot express the bits on disk).
var (
	quiesceJournalDirMode  = os.FileMode(0o700)
	quiesceJournalFileMode = os.FileMode(0o600)
)
var (
	// quiesceReadyCeiling bounds the wait for the script's READY marker
	// (download plus extraction of the whole archive). Generous on
	// purpose: giving up here is safe only because nothing has moved yet.
	// Vars so tests shrink them instead of leaning on the suite timeout.
	quiesceReadyCeiling = 2 * time.Hour
	// quiesceAbortWait bounds the wait for the job to take the ABORT hint
	// and exit. The script's own GO-watch loop polls in seconds; the bound
	// only catches a stuck container the teardown then removes.
	quiesceAbortWait = 10 * time.Minute
	// quiesceExitCeiling bounds the wait for the job to finish the data
	// exchange after the agent released it. The exchange copies the whole
	// application's data, so it is generous — but bounded: an unbounded
	// wait held the agent's command queue hostage. Past the ceiling the
	// exchange is treated as a failure after data moved, rolled back like
	// any other. Var so tests shrink it.
	quiesceExitCeiling = 2 * time.Hour
)

var (
	quiesceJobID    = regexp.MustCompile(`^bkpjob_[a-f0-9]{16}$`)
	quiesceBackupID = regexp.MustCompile(`^bkp_[a-f0-9]{16}$`)
	quiesceReplaced = regexp.MustCompile(`^data\.replaced-[0-9]{8}-[0-9]{6}$`)
)

func quiesceRestartOK(policy string) bool {
	return slices.Contains([]string{"", "no", "always", "unless-stopped", "on-failure"}, policy)
}

// quiescePlan is the validated quiesce request plus the identities the
// orchestration needs from the job's own variables, so the record on disk
// is self-contained for recovery.
type quiescePlan struct {
	Target      string
	BackupID    string
	StopTimeout int
}

// quiescePlanFor refuses the whole command when the quiesce block does not
// name exactly the job's own target. This runs before anything executes:
// a mismatched target would stop the wrong customer's application.
func (d *Docker) quiescePlanFor(p sdkclient.DeployPayload) (quiescePlan, error) {
	none := quiescePlan{}
	q := p.Quiesce
	if q == nil {
		return none, nil
	}
	if !quiesceJobID.MatchString(p.DeploymentID) {
		return none, errors.New("quiesce is only valid on a backup transport job deployment_id")
	}
	if !failoverDeploymentID.MatchString(q.Target) {
		return none, errors.New("quiesce target is not a deployment identity")
	}
	if p.Manifest.Runtime.Build != nil || p.Manifest.Runtime.BackupDatabase != nil || p.Manifest.Runtime.RestoreDatabase != nil || len(p.Routes) != 0 {
		return none, errors.New("quiesce cannot be combined with a build, database stage or routes")
	}
	target, _ := p.Vars["IMPREZA_TARGET"].(string)
	if target != q.Target {
		return none, errors.New("quiesce target does not match the job's IMPREZA_TARGET")
	}
	if replaced, _ := p.Vars["IMPREZA_REPLACED"].(string); replaced != "" {
		return none, errors.New("quiesce is not valid on a discard job")
	}
	backupID, _ := p.Vars["IMPREZA_JOB"].(string)
	if !quiesceBackupID.MatchString(backupID) {
		return none, errors.New("quiesce requires a well-formed IMPREZA_JOB")
	}
	if !exists(d.appDir(q.Target)) {
		return none, errors.New("quiesce target is not a deployment directory on this agent")
	}
	// A previous restore of this application ended quarantined with
	// a live job. A new restore would stop and swap the same data a job
	// from the quarantined restore may still be writing into; only support,
	// with the job dead, concludes that state.
	if journals, jerr := d.quiesceJournals(); jerr == nil {
		for _, jobID := range journals {
			existing, lerr := d.loadQuiesceRecord(jobID)
			if lerr != nil || existing == nil || existing.Phase != "quiesce_orphan" {
				continue
			}
			if existing.Target == q.Target {
				return none, fmt.Errorf("a previous restore of this application is quarantined (quiesce_orphan, job %s); it must be reconciled before a new restore", jobID)
			}
		}
	}
	timeout := q.StopTimeoutSeconds
	if timeout == 0 {
		timeout = quiesceStopDefault
	}
	if timeout < 10 || timeout > 600 {
		return none, errors.New("quiesce stop_timeout_seconds must be between 10 and 600")
	}
	return quiescePlan{Target: q.Target, BackupID: backupID, StopTimeout: timeout}, nil
}

// ─────────────────────────────────────────────────────────────────────
// journal
// ─────────────────────────────────────────────────────────────────────

// restoreQuiesceRecord is the durable state of one quiesce restore. It is
// written before the job container starts and removed only after the
// command's result is acknowledged, so every crash window resumes.
type restoreQuiesceRecord struct {
	Version        int    `json:"version"`
	CommandID      string `json:"command_id"`
	JobID          string `json:"job_id"`
	BackupID       string `json:"backup_id"`
	Target         string `json:"target"`
	StopTimeoutSec int    `json:"stop_timeout_seconds"`
	// Phase: staging (job running, application up), stopping (READY seen,
	// containers being stopped), stopped (verified stopped, GO released),
	// starting (job exited, application being brought back).
	Phase   string `json:"phase"`
	Outcome string `json:"outcome,omitempty"`
	// Undone records that a failed exchange was rolled back successfully;
	// recovery at phase starting rebuilds the receipt from it.
	Undone bool `json:"undone,omitempty"`
	// Containers holds the restart policy each container had before the
	// agent disabled it; startup gives the policies back.
	Containers []restoreQuiesceContainerPolicy `json:"containers,omitempty"`
	// BaselineReplaced names the displaced trees that existed before this
	// command ran; a tree outside it means this restore swapped data.
	BaselineReplaced []string `json:"baseline_replaced,omitempty"`
	JobExitCode      int      `json:"job_exit_code,omitempty"`
	StartedUTC       string   `json:"started_utc,omitempty"`
}

type restoreQuiesceContainerPolicy struct {
	ID      string `json:"id"`
	Restart string `json:"restart"`
}

func (r *restoreQuiesceRecord) validate() error {
	if r == nil || r.Version != 1 || len(r.CommandID) == 0 || len(r.CommandID) > 200 ||
		!quiesceJobID.MatchString(r.JobID) || !quiesceBackupID.MatchString(r.BackupID) ||
		!failoverDeploymentID.MatchString(r.Target) {
		return errors.New("invalid restore quiesce record identity")
	}
	if !slices.Contains([]string{"staging", "stopping", "stopped", "starting", "quiesce_orphan"}, r.Phase) {
		return errors.New("invalid restore quiesce phase")
	}
	if r.Outcome != "" && !slices.Contains([]string{"succeed", "fail_unchanged", "fail_undone"}, r.Outcome) {
		return errors.New("invalid restore quiesce outcome")
	}
	hasOutcome := r.Phase == "starting" || r.Phase == "quiesce_orphan"
	if hasOutcome != (r.Outcome != "") {
		return errors.New("restore quiesce outcome and phase disagree")
	}
	if r.StopTimeoutSec < 10 || r.StopTimeoutSec > 600 {
		return errors.New("invalid restore quiesce stop timeout")
	}
	seen := map[string]bool{}
	for _, c := range r.Containers {
		if !recoveryContainerID.MatchString(c.ID) || seen[c.ID] || !quiesceRestartOK(c.Restart) {
			return errors.New("invalid restore quiesce container policy")
		}
		seen[c.ID] = true
	}
	for _, name := range r.BaselineReplaced {
		if !quiesceReplaced.MatchString(name) {
			return errors.New("invalid restore quiesce baseline")
		}
	}
	return nil
}

func (d *Docker) quiesceDir(jobID string) (string, error) {
	if !quiesceJobID.MatchString(jobID) || !filepath.IsAbs(d.StateDir) {
		return "", errors.New("invalid restore quiesce journal path")
	}
	return filepath.Join(d.StateDir, "operations", "restore-"+jobID), nil
}

func (d *Docker) loadQuiesceRecord(jobID string) (*restoreQuiesceRecord, error) {
	dir, err := d.quiesceDir(jobID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "record.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("unsafe restore quiesce record file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r restoreQuiesceRecord
	if json.Unmarshal(raw, &r) != nil {
		return nil, errors.New("corrupt restore quiesce record")
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *Docker) saveQuiesceRecord(r restoreQuiesceRecord) error {
	if err := r.validate(); err != nil {
		return err
	}
	dir, err := d.quiesceDir(r.JobID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, quiesceJournalDirMode); err != nil {
		return err
	}
	if err := realWorkDirectory(dir); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "record.json"), raw, quiesceJournalFileMode); err != nil {
		return err
	}
	return syncRecoveryDirectory(dir)
}

// beginQuiesceRecord persists the command's identities before any Docker
// action, with the displaced-tree baseline that later decides whether a
// failure swapped data.
func (d *Docker) beginQuiesceRecord(jobID, commandID string, plan quiescePlan) error {
	if existing, err := d.loadQuiesceRecord(jobID); err != nil || existing != nil {
		return errors.New("a restore quiesce journal already exists for this job")
	}
	r := restoreQuiesceRecord{
		Version:        1,
		CommandID:      commandID,
		JobID:          jobID,
		BackupID:       plan.BackupID,
		Target:         plan.Target,
		StopTimeoutSec: plan.StopTimeout,
		Phase:          "staging",
		StartedUTC:     time.Now().UTC().Format(time.RFC3339),
	}
	names, err := d.quiesceReplacedNames(plan.Target)
	if err != nil {
		return err
	}
	r.BaselineReplaced = names
	return d.saveQuiesceRecord(r)
}

// ForgetRestoreQuiesce removes the journal of a command whose result the
// control plane acknowledged. The undo area goes with it: by then the
// outcome is recorded where the customer reads it.
func (d *Docker) ForgetRestoreQuiesce(commandID string) error {
	jobID, err := d.quiesceJobByCommand(commandID)
	if err != nil || jobID == "" {
		return err
	}
	return d.forgetQuiesceRecord(jobID)
}

func (d *Docker) forgetQuiesceRecord(jobID string) error {
	dir, err := d.quiesceDir(jobID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(dir))
}

// quiesceReplacedNames lists the displaced trees currently on disk.
func (d *Docker) quiesceReplacedNames(target string) ([]string, error) {
	entries, err := os.ReadDir(d.appDir(target))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if quiesceReplaced.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// quiesceNewReplaced returns the displaced tree this command created, or ""
// when none did. The newest wins if more than one appeared: a job swaps
// exactly once, and an older name means a concurrent writer worth logging.
func (d *Docker) quiesceNewReplaced(target string, baseline []string) (string, error) {
	names, err := d.quiesceReplacedNames(target)
	if err != nil {
		return "", err
	}
	var fresh []string
	for _, name := range names {
		if !slices.Contains(baseline, name) {
			fresh = append(fresh, name)
		}
	}
	if len(fresh) == 0 {
		return "", nil
	}
	if len(fresh) > 1 {
		d.Log.Warn("restore quiesce: more than one displaced tree appeared during the job", "count", len(fresh))
	}
	slices.Sort(fresh)
	return fresh[len(fresh)-1], nil
}

// ─────────────────────────────────────────────────────────────────────
// orchestration
// ─────────────────────────────────────────────────────────────────────

func quiescePending(cmd *sdkclient.PollCommand, jobID string) sdkclient.DeployResult {
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: PreparationPendingStatus, DeploymentID: jobID,
		Error: "restore quiesce operation was interrupted; recovery continues from the journal"}
}

// runQuiesceRestore is the live lead-in from deploy(): the job project
// goes up, then the shared phase machine owns everything to the terminal
// result, exactly as it would after an agent restart.
func (d *Docker) runQuiesceRestore(ctx context.Context, cmd *sdkclient.PollCommand, p sdkclient.DeployPayload) sdkclient.DeployResult {
	jobID := p.DeploymentID
	jobDir := d.appDir(jobID)
	d.deploymentProgress(ctx, cmd, "staging")
	upCtx, cancelUp := context.WithTimeout(ctx, composeUpTimeout)
	out, err := d.compose(upCtx, jobDir, "up", "-d", "--no-build", "--pull", "never")
	cancelUp()
	if err != nil {
		proof := &sdkclient.RestoreQuiesceResult{Target: p.Quiesce.Target, JobExitCode: -1}
		result := sdkclient.DeployResult{CommandID: cmd.ID, Status: "failed", DeploymentID: jobID,
			Error:          fmt.Sprintf("the restore job could not start: %v\n%s", err, tail(out, 1536)),
			RestoreQuiesce: proof}
		d.quiesceWriteResult(jobID, result)
		d.quiesceTearDown(ctx, jobDir)
		return result
	}
	if ctx.Err() != nil {
		return quiescePending(cmd, jobID)
	}
	return d.quiesceDrive(ctx, cmd, jobID)
}

// quiesceDrive is the resumable phase machine of one quiesce restore. It
// picks up from whatever the journal says lasted, which makes the crash
// windows of the live run and the recovery after an agent restart the
// same code path. Every terminal outcome is durably recorded before it
// is returned.
func (d *Docker) quiesceDrive(ctx context.Context, cmd *sdkclient.PollCommand, jobID string) sdkclient.DeployResult {
	record, err := d.loadQuiesceRecord(jobID)
	if err != nil || record == nil {
		return failResult(cmd.ID, "restore quiesce journal is unreadable; the operation is not repeated")
	}
	plan := quiescePlan{Target: record.Target, BackupID: record.BackupID, StopTimeout: record.StopTimeoutSec}
	jobDir := d.appDir(jobID)
	proof := &sdkclient.RestoreQuiesceResult{Target: plan.Target}
	// exchangeStalled marks a job that never finished the data exchange
	// within the ceiling (quiesceExitCeiling), with the application stopped.
	exchangeStalled := false
	proof.JobExitCode = record.JobExitCode
	terminal := func(status, msg string) sdkclient.DeployResult {
		result := sdkclient.DeployResult{CommandID: cmd.ID, Status: status, DeploymentID: jobID, RestoreQuiesce: proof}
		if status != "success" {
			result.Error = msg
		}
		d.quiesceWriteResult(jobID, result)
		return result
	}

	if record.Phase == "staging" {
		if states, err := d.inspectProjectContainers(ctx, jobID); err == nil && len(states) == 0 {
			// Resume from before the container existed: bring the job up
			// again and let the phases run from the top.
			upCtx, cancelUp := context.WithTimeout(ctx, composeUpTimeout)
			out, upErr := d.compose(upCtx, jobDir, "up", "-d", "--no-build", "--pull", "never")
			cancelUp()
			if upErr != nil {
				return terminal("failed", fmt.Sprintf("the restore job could not be started again: %v\n%s", upErr, tail(out, 1536)))
			}
		}
		ready, exitCode := d.quiesceAwaitReady(ctx, jobID, plan)
		if ctx.Err() != nil {
			return quiescePending(cmd, jobID)
		}
		if !ready {
			if exitCode >= 0 {
				return d.quiesceTerminalLogged(ctx, cmd, jobID, proof, exitCode,
					fmt.Sprintf("the restore failed before any data was touched (job exit %d); the application was not stopped and nothing was changed", exitCode))
			}
			return d.quiesceTerminalLogged(ctx, cmd, jobID, proof, -1,
				"the restore did not finish staging in time; the application was not stopped and nothing was changed")
		}
		record.Phase = "stopping"
		if err := d.saveQuiesceRecord(*record); err != nil {
			return terminal("failed", "restore quiesce journal could not be updated: "+err.Error())
		}
	}

	if record.Phase == "stopping" {
		// Phase 2 — stopping: journal first, then the durable restart
		// disable, the stop, and the verified-stopped check.
		d.deploymentProgress(ctx, cmd, "stopping")
		policies, stopErr := d.quiesceStopTarget(ctx, plan.Target, plan.StopTimeout)
		record.Containers = policies
		if stopErr != nil {
			// Some container refused to stop: no data may move. Persist
			// the abort decision before touching anything again, so a
			// crash here resumes into the same abort instead of retrying
			// the stop on an application that is already back up.
			d.Log.Warn("restore quiesce: target did not stop; aborting before any data moves", "target", plan.Target, "err", stopErr)
			record.Phase = "starting"
			record.Outcome = "fail_unchanged"
			record.JobExitCode = -1
			if err := d.saveQuiesceRecord(*record); err != nil {
				return terminal("failed", "restore quiesce journal could not be updated: "+err.Error())
			}
			d.quiesceSignalAbort(ctx, plan)
			d.quiesceRestorePolicies(ctx, policies)
			startErr := d.quiesceStartTarget(ctx, plan.Target)
			d.quiesceAwaitJobExit(ctx, jobID, quiesceAbortWait)
			msg := "the application could not be stopped for the restore (" + stopErr.Error() + "); nothing was changed"
			if startErr != nil {
				msg += ", but bringing it back up failed: " + startErr.Error()
			} else {
				msg += " and the application was left running"
			}
			return d.quiesceTerminalLogged(ctx, cmd, jobID, proof, -1, msg)
		}
		// Phase 3 — swapping: only after every container is verified
		// stopped does the agent release the exchange.
		d.deploymentProgress(ctx, cmd, "target_stopped")
		proof.Stopped = true
		record.Phase = "stopped"
		if err := d.saveQuiesceRecord(*record); err != nil {
			// The stop already happened; without a journal the recovery
			// cannot give the policies back, so finish synchronously.
			d.Log.Error("restore quiesce: journal write failed after the verified stop; completing without recovery safety", "err", err)
		}
	}

	if record.Phase == "stopped" {
		if err := d.quiesceSignalGo(ctx, plan); err != nil {
			// The script's own wait ceiling then ends the job unchanged;
			// the displaced-tree check still interprets the leftovers.
			d.Log.Warn("restore quiesce: GO marker could not be written; the job will time out unchanged", "err", err)
		}
		d.deploymentProgress(ctx, cmd, "swapping")
		exitCode := d.quiesceAwaitJobExit(ctx, jobID, quiesceExitCeiling)
		if exitCode < 0 && ctx.Err() == nil {
			exchangeStalled = true
		}
		if ctx.Err() != nil {
			return quiescePending(cmd, jobID)
		}
		if exchangeStalled {
			// The exchange never finished inside the ceiling: the job
			// container is stuck mid-copy with the application stopped.
			// Tear it down FIRST and verify nothing of the job project is
			// still running — a live job keeps writing into data/ while
			// the rollback moves trees, mixing the two generations.
			d.Log.Error("restore quiesce: the data exchange did not finish within the ceiling; tearing the job down", "job", jobID, "ceiling", quiesceExitCeiling.String())
			d.quiesceTearDown(ctx, jobDir)
			if running := d.quiesceJobStillRunning(ctx, jobID); running {
				// The job would not die. Nothing may be undone and the
				// application must not start on data a live job is still
				// writing into. This is terminal: no automatic path —
				// not the reconcile, not the owner's resume, not a new
				// restore — may conclude it. The application stays
				// stopped until support or the client acts with the job
				// dead.
				record.Phase = "quiesce_orphan"
				record.Outcome = "fail_unchanged"
				record.JobExitCode = exitCode
				if err := d.saveQuiesceRecord(*record); err != nil {
					d.Log.Error("restore quiesce: journal write failed", "err", err)
				}
				result := sdkclient.DeployResult{CommandID: cmd.ID, Status: "failed", DeploymentID: jobID,
					RestoreQuiesce: proof,
					Error:          fmt.Sprintf("the data exchange did not finish within %s and the job could not be stopped; nothing was undone and the application remains stopped. The restore journal is quarantined (quiesce_orphan) until support confirms the job is dead. Contact support before restarting it.", quiesceExitCeiling)}
				d.quiesceWriteResult(jobID, result)
				return result
			}
		}
		proof.JobExitCode = exitCode
		record.JobExitCode = exitCode
		replaced, replacedErr := d.quiesceNewReplaced(plan.Target, record.BaselineReplaced)
		if replacedErr != nil {
			replaced = ""
			d.Log.Warn("restore quiesce: displaced tree could not be inspected", "err", replacedErr)
		}
		// Phase 4 — interpret the outcome; undo a failed exchange before
		// the application can read a half-restored tree. The undo only
		// runs with the job confirmed dead (the stall path above tears
		// down first; the normal exit paths have the container finished).
		switch {
		case exitCode == 0:
			record.Outcome = "succeed"
		case exchangeStalled || exitCode == quiesceExitSwapped || replaced != "":
			record.Outcome = "fail_undone"
			if undoErr := d.quiesceUndoSwap(jobID, plan.Target, replaced); undoErr != nil {
				d.Log.Error("restore quiesce: swap rollback failed; the application may start on mixed data", "err", undoErr)
			} else {
				record.Undone = true
				proof.Undone = true
			}
		default:
			record.Outcome = "fail_unchanged"
		}
		record.Phase = "starting"
		if err := d.saveQuiesceRecord(*record); err != nil {
			d.Log.Error("restore quiesce: journal write failed before startup", "err", err)
		}
	}

	if record.Phase == "quiesce_orphan" {
		// Terminal: the job survived the teardown and the application
		// must not start on data a live job is still writing into. No
		// automatic path — the owner's resume, the reconcile, or a new
		// restore — may conclude this state. The result is
		// already recorded; re-answer from it.
		if recorded, ok := d.quiesceReadResult(jobID, cmd.ID); ok {
			return recorded
		}
		proof.Stopped = true
		proof.JobExitCode = record.JobExitCode
		return sdkclient.DeployResult{CommandID: cmd.ID, Status: "failed", DeploymentID: jobID,
			RestoreQuiesce: proof,
			Error:          "the restore journal is quarantined (quiesce_orphan): the job could not be stopped and the application remains stopped. Contact support before restarting it."}
	}

	// Phase starting — bring the application back and settle it.
	d.deploymentProgress(ctx, cmd, "starting")
	logs := d.quiesceJobLogs(ctx, jobDir)
	d.quiesceTearDown(ctx, jobDir)
	if !(record.Outcome == "fail_unchanged" && record.JobExitCode < 0) {
		// The abort path (exit -1, nothing changed) is the only outcome
		// whose stop never completed.
		proof.Stopped = true
	}
	note, receipt := d.quiesceStartTargetVerified(ctx, plan.Target, record.Containers)
	switch record.Outcome {
	case "succeed":
		result := sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: jobID,
			RestoreQuiesce: proof, StartupCheck: receipt,
			LogsTail: "health: " + note + "\n\n" + tail([]byte(logs), 4096)}
		d.quiesceWriteResult(jobID, result)
		return result
	case "fail_undone":
		if record.Undone {
			if exchangeStalled {
				return terminal("failed", fmt.Sprintf("the data exchange did not finish within %s and was rolled back; the previous data is back in place. %s\n%s", quiesceExitCeiling, note, logs))
			}
			return terminal("failed", "the restore job failed after the data exchange started; the exchange was undone and the previous data put back. "+note+"\n"+logs)
		}
		if exchangeStalled {
			return terminal("failed", fmt.Sprintf("the data exchange did not finish within %s and the rollback did not complete; contact support before restarting the application. %s\n%s", quiesceExitCeiling, note, logs))
		}
		return terminal("failed", "the restore job failed after the data exchange started and the rollback did not complete; contact support before restarting the application. "+note+"\n"+logs)
	default:
		if record.JobExitCode >= 0 {
			return terminal("failed", fmt.Sprintf("the restore failed (job exit %d) without changing any data. %s\n%s", record.JobExitCode, note, logs))
		}
		return terminal("failed", "the restore was aborted before any data was touched. "+note+"\n"+logs)
	}
}

// quiesceTerminalLogged finishes with the job's log tail attached and the
// project torn down before the result is durably recorded.
func (d *Docker) quiesceTerminalLogged(ctx context.Context, cmd *sdkclient.PollCommand, jobID string, proof *sdkclient.RestoreQuiesceResult, exitCode int, msg string) sdkclient.DeployResult {
	proof.JobExitCode = exitCode
	logs := d.quiesceJobLogs(ctx, d.appDir(jobID))
	d.quiesceTearDown(ctx, d.appDir(jobID))
	result := sdkclient.DeployResult{CommandID: cmd.ID, Status: "failed", DeploymentID: jobID,
		Error: msg + "\n" + logs, RestoreQuiesce: proof}
	d.quiesceWriteResult(jobID, result)
	return result
}

// quiesceAwaitReady waits for the script's READY marker while the
// application keeps running. Returns ready=false with the job's exit code
// when the job container finished first, or -1 when staging ran out.
func (d *Docker) quiesceAwaitReady(ctx context.Context, jobID string, plan quiescePlan) (bool, int) {
	readyPath := filepath.Join(d.appDir(plan.Target), ".restore-"+plan.BackupID, "READY")
	deadline := time.Now().Add(quiesceReadyCeiling)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	misses := 0
	for {
		if ctx.Err() != nil {
			return false, -1
		}
		if exists(readyPath) {
			return true, 0
		}
		states, err := d.inspectProjectContainers(ctx, jobID)
		if err != nil {
			misses++
		} else if len(states) == 0 {
			// `up` returned 0 moments ago; a persistently empty set means
			// the container vanished without leaving a code behind.
			misses++
		} else {
			misses = 0
			if quiesceAllExited(states) {
				return false, quiesceJobExit(states)
			}
		}
		if misses > 30 {
			d.Log.Error("restore quiesce: the job could not be observed during staging; nothing was changed", "err", err)
			return false, -1
		}
		if time.Now().After(deadline) {
			d.Log.Error("restore quiesce: staging did not finish in time; nothing was changed", "job", jobID)
			return false, -1
		}
		select {
		case <-ctx.Done():
			return false, -1
		case <-time.After(quiescePollInterval):
		}
	}
}

// quiesceAwaitJobExit blocks until every job container has terminated and
// returns the job service's exit code, or -1 when the container vanished
// before a code could be read. A zero ceiling waits without a bound: once
// the exchange is released the agent must not abandon a moving tree.
func (d *Docker) quiesceAwaitJobExit(ctx context.Context, jobID string, ceiling time.Duration) int {
	deadline := time.Time{}
	if ceiling > 0 {
		deadline = time.Now().Add(ceiling)
	}
	misses := 0
	for {
		if ctx.Err() != nil {
			return -1
		}
		states, err := d.inspectProjectContainers(ctx, jobID)
		if err != nil || len(states) == 0 {
			misses++
			if misses > 30 {
				return -1
			}
		} else {
			misses = 0
			if quiesceAllExited(states) {
				return quiesceJobExit(states)
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return -1
		}
		select {
		case <-ctx.Done():
			return -1
		case <-time.After(quiescePollInterval):
		}
	}
}

func quiesceAllExited(states []containerState) bool {
	for _, s := range states {
		if s.Status != "exited" && s.Status != "dead" {
			return false
		}
	}
	return true
}

// quiesceJobExit reads the exit code of the transport service's own
// container, identified by the container name prefix the control plane
// fixes for every transport job.
func quiesceJobExit(states []containerState) int {
	code := -1
	for _, s := range states {
		if strings.HasPrefix(s.Name, "impreza_backup_") {
			return s.ExitCode
		}
		code = s.ExitCode
	}
	return code
}

// quiesceStopTarget disables restart durably, stops every container of the
// target and verifies the set is down — the fence's verified stop,
// generalized to a restore target and returning the policies it changed.
// The returned policies cover the containers already disabled when an
// error interrupts the sequence, so the caller can give them back.
func (d *Docker) quiesceStopTarget(ctx context.Context, target string, timeoutSeconds int) ([]restoreQuiesceContainerPolicy, error) {
	ids, err := d.recoveryContainers(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(ids) > 256 {
		return nil, errors.New("too many containers to quiesce")
	}
	type observed struct {
		ID         string
		Config     struct{ Labels map[string]string }
		State      struct{ Running, Restarting bool }
		HostConfig struct{ RestartPolicy struct{ Name string } }
	}
	inspect := func(cid string) (observed, error) {
		raw, err := limitedRuntimeOutput(d.dockerCmd(ctx, "inspect", cid), 1024*1024)
		var rows []observed
		if err != nil || json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || rows[0].ID != cid || rows[0].Config.Labels["com.docker.compose.project"] != target {
			return observed{}, errors.New("quiesced container ownership changed")
		}
		return rows[0], nil
	}
	var policies []restoreQuiesceContainerPolicy
	for _, cid := range ids {
		state, err := inspect(cid)
		if err != nil {
			return policies, err
		}
		if _, err = limitedRuntimeOutput(d.dockerCmd(ctx, "update", "--restart=no", cid), 4096); err != nil {
			return policies, err
		}
		policies = append(policies, restoreQuiesceContainerPolicy{ID: cid, Restart: state.HostConfig.RestartPolicy.Name})
		if _, err = limitedRuntimeOutput(d.dockerCmd(ctx, "stop", "--time", strconv.Itoa(timeoutSeconds), cid), 4096); err != nil {
			return policies, err
		}
		after, err := inspect(cid)
		if err != nil {
			return policies, err
		}
		if after.State.Running || after.State.Restarting || after.HostConfig.RestartPolicy.Name != "no" {
			return policies, errors.New("quiesced container is still running or restartable")
		}
	}
	final, err := d.recoveryContainers(ctx, target)
	if err != nil {
		return policies, err
	}
	if !slices.Equal(ids, final) {
		return policies, errors.New("target containers changed during the stop")
	}
	return policies, nil
}

// quiesceRestorePolicies gives each container back the restart policy the
// stop disabled. Best-effort by design: it runs on paths where the
// containers may already be gone.
func (d *Docker) quiesceRestorePolicies(ctx context.Context, policies []restoreQuiesceContainerPolicy) {
	for _, c := range policies {
		restart := c.Restart
		if restart == "" {
			restart = "no"
		}
		if _, err := limitedRuntimeOutput(d.dockerCmd(ctx, "update", "--restart="+restart, c.ID), 4096); err != nil {
			d.Log.Warn("restore quiesce: restart policy could not be returned", "container", c.ID, "err", err)
		}
	}
}

// quiesceSignalGo releases the swap. Written only after every container of
// the target was verified stopped.
func (d *Docker) quiesceSignalGo(ctx context.Context, plan quiescePlan) error {
	dir := filepath.Join(d.appDir(plan.Target), ".restore-"+plan.BackupID)
	if err := realWorkDirectory(dir); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "GO"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// quiesceSignalAbort tells a waiting script to stand down and clean its
// staging area. A script already past its wait ignores it, which the exit
// code and the displaced-tree check on disk still interpret correctly.
func (d *Docker) quiesceSignalAbort(ctx context.Context, plan quiescePlan) {
	dir := filepath.Join(d.appDir(plan.Target), ".restore-"+plan.BackupID)
	if err := realWorkDirectory(dir); err != nil {
		return
	}
	if err := writeAtomic(filepath.Join(dir, "ABORT"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		d.Log.Warn("restore quiesce: ABORT marker could not be written; the job will time out unchanged", "err", err)
	}
}

// quiesceStartTargetVerified restarts the application and settles it,
// returning the human-readable startup note for the result and the startup
// receipt when a required health check passed. Startup problems are
// reported in the note; the caller decides the final status.
func (d *Docker) quiesceStartTargetVerified(ctx context.Context, target string, policies []restoreQuiesceContainerPolicy) (string, *sdkclient.DeploymentStartupCheck) {
	d.quiesceRestorePolicies(ctx, policies)
	if err := d.quiesceStartTarget(ctx, target); err != nil {
		return "bringing the application back up failed: " + err.Error(), nil
	}
	policy := startupPolicy{Timeout: settleBudget}
	if manifest, err := readStartupPolicy(d.appDir(target)); err == nil && manifest != nil {
		if resolved, err := resolveStartupPolicy(manifest); err == nil {
			policy = resolved
		}
	}
	verdict, detail := d.awaitStackSettledPolicy(ctx, target, policy)
	switch verdict {
	case settleCrashLooping:
		return "the application did not stay up after the restore: " + detail, nil
	case settleUnsettled:
		if policy.RequireHealthy {
			return "the application did not pass its required startup checks: " + detail, nil
		}
		return "the application was started but not confirmed healthy (advisory): " + detail, nil
	default:
		return "the application is up: " + detail, policy.receipt(verdict)
	}
}

// quiesceStartTarget brings the target's containers back with their
// existing configuration: no build, no pull, no recreate.
func (d *Docker) quiesceStartTarget(ctx context.Context, target string) error {
	if d.Tor != nil && d.Tor.HasService(target) {
		// The restore may have put a previous .onion key back while Tor
		// kept running; provisioning is idempotent and reloads the
		// service so the restored identity is the one published.
		torCtx, cancelTor := context.WithTimeout(ctx, 3*time.Minute)
		_, err := d.Tor.ProvisionHiddenService(torCtx, target, 80)
		cancelTor()
		if err != nil {
			d.Log.Warn("restore quiesce: hidden service could not be reloaded", "target", target, "err", err)
		}
	}
	upCtx, cancelUp := context.WithTimeout(ctx, composeUpTimeout)
	out, err := d.compose(upCtx, d.appDir(target), "up", "-d", "--no-build", "--pull", "never")
	cancelUp()
	if err != nil {
		return fmt.Errorf("docker compose up: %v\n%s", err, tail(out, 1536))
	}
	return nil
}

// quiesceJobLogs grabs a short tail of the job's own output for the
// result body, before the teardown removes the container.
func (d *Docker) quiesceJobLogs(ctx context.Context, jobDir string) string {
	logsCtx, cancelLogs := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancelLogs()
	out, _ := d.compose(logsCtx, jobDir, "logs", "--tail=40", "--no-color")
	return tail(out, 4096)
}

// quiesceTearDown removes the job project's container and network. The
// image stays: the next job must not pay the registry again, and no
// customer data ever lived in this project's own volumes.
func (d *Docker) quiesceTearDown(ctx context.Context, jobDir string) {
	downCtx, cancelDown := context.WithTimeout(ctx, composeDownTimeout)
	defer cancelDown()
	if out, err := d.compose(downCtx, jobDir, "down"); err != nil {
		d.Log.Warn("restore quiesce: job project teardown failed", "err", err, "out", tail(out, 512))
	}
}

// quiesceJobStillRunning reports whether any container of the job project
// is still alive. The undo of a stalled exchange only runs when this is
// false: a live job keeps writing into data/ and would mix the two
// generations mid-rollback.
func (d *Docker) quiesceJobStillRunning(ctx context.Context, jobID string) bool {
	states, err := d.inspectProjectContainers(ctx, jobID)
	if err != nil {
		// Cannot verify: assume it is running, the safe side.
		return true
	}
	for _, s := range states {
		if s.Status == "running" || s.Status == "restarting" || s.Status == "paused" {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// undo
// ─────────────────────────────────────────────────────────────────────

// quiesceUndoSwap puts the application's previous data back after an
// exchange that failed partway, with the application stopped. Each area
// (data directory, each named volume, the .onion key directory) is
// evacuated before its previous contents return, and each pass persists
// its own completion marker so an interrupted undo resumes instead of
// mixing the two generations. The rejected generation stays parked under
// the job's journal until the journal is forgotten.
func (d *Docker) quiesceUndoSwap(jobID, target, replaced string) error {
	if replaced == "" {
		return errors.New("no displaced tree to roll back to")
	}
	dir, err := d.quiesceDir(jobID)
	if err != nil {
		return err
	}
	undoDir := filepath.Join(dir, "undo")
	if err := os.MkdirAll(undoDir, quiesceJournalDirMode); err != nil {
		return err
	}
	appData := filepath.Join(d.appDir(target), "data")
	oldRoot := filepath.Join(d.appDir(target), replaced)

	// 1. The bind-mounted data directory. The parking subdirectories the
	// script itself uses for volumes and the .onion key are not app data
	// and belong to their own areas below.
	if err := quiesceUndoArea(undoDir, "data", appData, oldRoot, "vol", "onion"); err != nil {
		return err
	}
	// 2. The named volumes the job swapped through /tvol.
	volRoot := filepath.Join(oldRoot, "vol")
	if exists(volRoot) {
		entries, err := os.ReadDir(volRoot)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			mount, err := d.quiesceVolumeMountpoint(e.Name(), target)
			if err != nil {
				return err
			}
			if err := quiesceUndoArea(filepath.Join(undoDir, "vol"), e.Name(), mount, filepath.Join(volRoot, e.Name())); err != nil {
				return err
			}
		}
	}
	// 3. The .onion key directory.
	if exists(filepath.Join(oldRoot, "onion")) {
		serviceDir := filepath.Join(d.StateDir, "proxy", "tor", "services", target)
		if err := quiesceUndoArea(undoDir, "onion", serviceDir, filepath.Join(oldRoot, "onion")); err != nil {
			return err
		}
		if err := os.Chmod(serviceDir, 0o700); err != nil {
			d.Log.Warn("restore quiesce: service directory mode could not be reset", "err", err)
		}
	}
	return syncRecoveryDirectory(dir)
}

// quiesceUndoArea reverses one swapped directory in two idempotent passes:
// first park whatever the failed exchange left in place, marked done when
// the live directory is empty; then move the previous contents back,
// marked done when the parked source has nothing left to give (the skip
// names are the parking subdirectories the script owns). Resuming a
// finished pass is a no-op, so a crash between moves cannot interleave
// generations.
func quiesceUndoArea(undoRoot, name, live, parked string, skip ...string) error {
	park := filepath.Join(undoRoot, name)
	if err := os.MkdirAll(park, quiesceJournalDirMode); err != nil {
		return err
	}
	if !exists(filepath.Join(undoRoot, "pass1-"+name+".done")) {
		entries, err := os.ReadDir(live)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := quiesceMove(filepath.Join(live, e.Name()), filepath.Join(park, e.Name())); err != nil {
				return err
			}
		}
		if err := writeAtomic(filepath.Join(undoRoot, "pass1-"+name+".done"), []byte("done\n"), 0o600); err != nil {
			return err
		}
	}
	if !exists(filepath.Join(undoRoot, "pass2-"+name+".done")) {
		entries, err := os.ReadDir(parked)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if slices.Contains(skip, e.Name()) {
				continue
			}
			if err := quiesceMove(filepath.Join(parked, e.Name()), filepath.Join(live, e.Name())); err != nil {
				return err
			}
		}
		if err := writeAtomic(filepath.Join(undoRoot, "pass2-"+name+".done"), []byte("done\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// quiesceMove moves one tree, falling back to a mode- and owner-preserving
// copy when source and destination live on different filesystems (a named
// volume's mountpoint versus the application directory).
func quiesceMove(source, destination string) error {
	err := os.Rename(source, destination)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	return quiesceCopyTree(source, destination)
}

func quiesceCopyTree(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := quiesceCopyTree(filepath.Join(source, e.Name()), filepath.Join(destination, e.Name())); err != nil {
				return err
			}
		}
		quiescePreserveOwner(destination, info)
		return os.Remove(source)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, destination); err != nil {
			return err
		}
		return os.Remove(source)
	default:
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, info.Mode().Perm()); err != nil {
			return err
		}
		quiescePreserveOwner(destination, info)
		return os.Remove(source)
	}
}

func (d *Docker) quiesceVolumeMountpoint(volume, target string) (string, error) {
	name := target + "_" + volume
	ctx, cancel := context.WithTimeout(context.Background(), composeQueryTimeout)
	defer cancel()
	out, err := limitedRuntimeOutput(d.dockerCmd(ctx, "volume", "inspect", "--format", "{{.Mountpoint}}", name), 4096)
	if err != nil {
		return "", fmt.Errorf("volume %s could not be resolved: %w", name, err)
	}
	mount := strings.TrimSpace(string(out))
	if !filepath.IsAbs(mount) || strings.Contains(mount, "..") {
		return "", fmt.Errorf("volume %s reported an unsafe mountpoint", name)
	}
	return mount, nil
}
