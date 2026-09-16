package executor

import (
	"context"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func ownedWorkerInactive(ctx context.Context, w *PreparationWork) (bool, error) {
	if err := w.validate(); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "show", "impreza-preparation-"+w.ID+".service", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=ControlPID", "--property=LoadState")
	out := &ownedCommandOutput{}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil || out.overflow {
		return false, errors.New("worker inactivity could not be verified")
	}
	return parseOwnedWorkerInactive(string(out.data))
}
func parseOwnedWorkerInactive(raw string) (bool, error) {
	if len(raw) > 65536 {
		return false, errors.New("worker state exceeds limit")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return false, errors.New("invalid worker state")
		}
		if _, exists := fields[k]; exists {
			return false, errors.New("duplicate worker state")
		}
		fields[k] = v
	}
	if len(fields) != 5 {
		return false, errors.New("incomplete worker state")
	}
	for _, k := range []string{"ActiveState", "SubState", "MainPID", "ControlPID", "LoadState"} {
		if _, ok := fields[k]; !ok {
			return false, errors.New("missing worker state")
		}
	}
	inactive := (fields["ActiveState"] == "inactive" && fields["SubState"] == "dead") || (fields["ActiveState"] == "failed" && fields["SubState"] == "failed")
	return inactive && fields["MainPID"] == "0" && fields["ControlPID"] == "0" && (fields["LoadState"] == "loaded" || fields["LoadState"] == "not-found"), nil
}

// RecoverOwnedPreparation is a failed-preparation recovery, not a cancellation
// grant or permission to replay. It is used only by resumed polling. A started
// marker prevents a second worker, systemd must prove inactivity, and the control
// plane must authorize this exact command in preparing phase before any mutation.
func (d *Docker) RecoverOwnedPreparation(ctx context.Context, cmd *sdkclient.PollCommand, w *PreparationWork, id string) (bool, error) {
	if cmd == nil || w == nil || cmd.ID != w.CommandID {
		return false, errors.New("recovery belongs to another command")
	}
	dir, request, err := d.loadPreparationWork(w, id)
	if err != nil {
		return false, err
	}
	if !request.OwnedBuilder {
		return false, nil
	}
	if _, err = d.preparationWorkResult(w, id); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrPreparationPending) {
		return false, err
	}
	inactive, err := ownedWorkerInactive(ctx, w)
	if err != nil || !inactive {
		return false, err
	}
	dir, request, daemon, run, err := d.ownedPreparationRuntimeBoot(w, id, true)
	if err != nil {
		return false, err
	}
	if cmd.ControlToken == "" || d.Client == nil {
		return false, errors.New("recovery requires authenticated control")
	}
	record, err := loadOwnedBuilderRecord(dir, w, id, request.BootID, daemon)
	if err != nil {
		return false, err
	}
	if record.Phase == "intent" && !record.CIDFile {
		return false, errors.New("unbound executor creation requires manual review")
	}
	if err = d.deploymentCheckpoint(ctx, cmd, "preparing"); err != nil && !errors.Is(err, errDeployCancelled) {
		return false, err
	}
	err = withOwnedBuilderLock(dir, func() error {
		inactive, err := ownedWorkerInactive(ctx, w)
		if err != nil {
			return err
		}
		if !inactive {
			return errors.New("worker became active during recovery")
		}
		info, err := os.Lstat(filepath.Join(dir, "started"))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 {
			return errors.New("missing exclusive worker start proof")
		}
		if _, err = os.Lstat(filepath.Join(dir, "result.json")); !os.IsNotExist(err) {
			return errors.New("worker receipt changed during recovery")
		}
		app, err := d.recoveryPath(id)
		if err != nil {
			return err
		}
		hash, err := preparationConfigHash(app)
		if err != nil || hash != request.ConfigSHA256 {
			return errors.New("preparation inputs changed before recovery")
		}
		current, err := loadOwnedBuilderRecord(dir, w, id, request.BootID, daemon)
		if err != nil {
			return err
		}
		if current.Phase == "intent" {
			if !current.CIDFile || current.Profile != "loaded" {
				return errors.New("unbound executor lacks a recoverable creation receipt")
			}
			cid, err := readOwnedBuilderCID(dir)
			if err != nil {
				return err
			}
			b, err := inspectOwnedBuilderCandidate(ctx, current, cid, run)
			if err != nil {
				return err
			}
			current.Identity, current.Phase = b, "bound"
			if err = current.validate(w, id, request.BootID, daemon); err != nil {
				return err
			}
			// Persist the verified full identity before any destructive action.
			if err = writeWorkJSON(dir, "builder.json", current); err != nil {
				return err
			}
		}
		if current.Phase != "recovering" && current.Phase != "recovered" {
			current.Phase = "recovering"
			current.RecoveryBootID = currentBootID()
			if err = current.validate(w, id, request.BootID, daemon); err != nil {
				return err
			}
			if err = writeWorkJSON(dir, "builder.json", current); err != nil {
				return err
			}
		}
		absent, err := ownedBuilderAbsent(ctx, current.Identity, run)
		if err != nil {
			return err
		}
		if !absent {
			if current.Phase == "recovered" {
				return errors.New("recovered executor reappeared")
			}
			if err = removeOwnedBuilder(ctx, current.Identity, w, id, run); err != nil {
				return err
			}
		}
		if request.PrivateBuild {
			if err = clearBuildSecrets(app); err != nil {
				return errors.New("recovery credential cleanup failed")
			}
		}
		current.Phase = "recovered"
		if err = writeWorkJSON(dir, "builder.json", current); err != nil {
			return err
		}
		if current.Profile != "" {
			if err = manageOwnedBuilderProfile(ctx, dir, current, false); err != nil {
				return err
			}
		}
		result := preparationWorkResult{Version: 1, ID: w.ID, RequestSHA256: w.RequestSHA256, Completed: true, RecoveryBootID: current.RecoveryBootID, Output: "Interrupted preparation was recovered without replay; retry explicitly."}
		return writeWorkJSON(dir, "result.json", result)
	})
	return err == nil, err
}

func (d *Docker) confirmOwnedRecovery(w *PreparationWork, id, boot string) error {
	dir, request, daemon, run, err := d.ownedPreparationRuntimeBoot(w, id, true)
	if err != nil {
		return err
	}
	record, err := loadOwnedBuilderRecord(dir, w, id, request.BootID, daemon)
	if err != nil {
		return err
	}
	if record.Phase != "recovered" || record.RecoveryBootID != boot || (record.Profile != "" && record.Profile != "released") {
		return errors.New("recovered receipt lacks terminal executor proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	absent, err := ownedBuilderAbsent(ctx, record.Identity, run)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("recovered executor is still present")
	}
	return nil
}
