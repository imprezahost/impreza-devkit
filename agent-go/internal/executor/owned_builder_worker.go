package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Each opted-in build owns its executor and must prove cleanup before receipt.
func (d *Docker) runOwnedPreparation(ctx context.Context, w *PreparationWork, request *preparationWorkRequest, dir, app string) (retErr error) {
	secretsHandled := false
	defer func() {
		// Prerequisite/create failures happen before the ordinary finalizer, but
		// must not strand staged credentials while the uncertain journal is kept.
		if request.PrivateBuild && !secretsHandled {
			if err := clearBuildSecrets(app); err != nil {
				retErr = errors.New("build credential cleanup failed")
			}
		}
	}()
	_, r, daemon, run, err := d.ownedPreparationRuntime(w, request.DeploymentID)
	if err != nil {
		return err
	}
	record, err := ensureOwnedBuilder(ctx, dir, w, r, daemon, run)
	if err != nil {
		return err
	}
	if record.Phase != "bound" && record.Phase != "stopping" && record.Phase != "stopped" {
		return errors.New("owned executor not ready; worker will not replay")
	}
	output := &preparationTail{limit: 4096}
	buildErr := func() error {
		// A controller may remove the executor before this inspection. Even an
		// early build failure must reach the locked stop/absence finalizer.
		if record.Phase != "bound" {
			return errDeployCancelled
		}
		raw, err := run(ctx, "container", "inspect", record.Identity.ContainerID)
		if err != nil {
			return err
		}
		if err = record.Identity.verifyInspection(raw); err != nil {
			return err
		}
		if err = os.Mkdir(filepath.Join(dir, "buildx"), 0700); err != nil {
			return err
		}
		env := ownedBuildEnvironment(request, dir)
		invoke := func(args ...string) error {
			cmd := exec.CommandContext(ctx, request.Docker, args...)
			cmd.Dir, cmd.Env = app, env
			cmd.Stdout, cmd.Stderr = output, output
			if request.PrivateBuild {
				cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			}
			return cmd.Run()
		}
		name := "impreza-work-" + w.ID
		if err = invoke("buildx", "create", "--name", name, "--driver", "remote", "--driver-opt", "default-load=true", "docker-container://"+record.Identity.ContainerID); err != nil {
			return err
		}
		// A removed CID cannot be recreated by the remote driver.
		args := []string{"compose", "build", "--builder", name}
		if request.PrivateBuild {
			args = append(args, "--no-cache")
		}
		return invoke(args...)
	}()
	var secretCleanupErr error
	if request.PrivateBuild {
		output.data = []byte("Build output withheld because private credentials were mounted.")
		secretsHandled = true
		if cleanupErr := clearBuildSecrets(app); cleanupErr != nil {
			secretCleanupErr = errors.New("build credential cleanup failed")
		}
	}
	// Docker client exit is not proof the build stopped. Finalization shares the
	// controller lock and must remove/verify the owned executor before a receipt.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := preparationWorkResult{Version: 1, ID: w.ID, RequestSHA256: w.RequestSHA256, Output: string(output.data)}
	for {
		err = withOwnedBuilderLock(dir, func() error {
			current, err := loadOwnedBuilderRecord(dir, w, r.DeploymentID, r.BootID, daemon)
			if err != nil {
				return err
			}
			if current.Phase == "stopping" {
				return syscall.EWOULDBLOCK
			}
			if current.Phase == "stopped" {
				absent, err := ownedBuilderAbsent(cleanupCtx, current.Identity, run)
				if err != nil {
					return err
				}
				if !absent {
					return errors.New("interrupted executor still present")
				}
				result.Completed = true
				result.Interrupted = true
			} else if current.Phase == "bound" {
				if err = removeOwnedBuilder(cleanupCtx, current.Identity, w, r.DeploymentID, run); err != nil {
					return err
				}
				current.Phase = "finished"
				if err = writeWorkJSON(dir, "builder.json", current); err != nil {
					return err
				}
				result.Completed = true
				result.Success = buildErr == nil && ctx.Err() == nil
			} else {
				return errors.New("owned worker finalization state is ambiguous")
			}
			if secretCleanupErr != nil {
				return secretCleanupErr
			}
			if current.Profile != "" {
				if err = manageOwnedBuilderProfile(cleanupCtx, dir, current, false); err != nil {
					return err
				}
			}
			return writeWorkJSON(dir, "result.json", result)
		})
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-cleanupCtx.Done():
			return errors.New("executor stop remained uncertain; no worker receipt")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
