package executor

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// These entry points require the opt-in request hash AND durable journal;
// a legacy build can never be interrupted here.
func (d *Docker) ownedPreparationRuntime(w *PreparationWork, id string) (string, *preparationWorkRequest, string, ownedBuilderCommand, error) {
	return d.ownedPreparationRuntimeBoot(w, id, false)
}
func (d *Docker) ownedPreparationRuntimeBoot(w *PreparationWork, id string, recovery bool) (string, *preparationWorkRequest, string, ownedBuilderCommand, error) {
	dir, r, err := d.loadPreparationWork(w, id)
	if err != nil {
		return "", nil, "", nil, err
	}
	if !r.OwnedBuilder || currentBootID() == "" || (!recovery && r.BootID != currentBootID()) {
		return "", nil, "", nil, errors.New("owned build is not bound to this host boot")
	}
	if err = localPreparationDaemon(r); err != nil {
		return "", nil, "", nil, err
	}
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, r.Docker, args...)
		cmd.Env = r.Env
		out := &ownedCommandOutput{}
		cmd.Stdout = out
		err := cmd.Run()
		if out.overflow {
			return nil, errors.New("Docker control output exceeded limit")
		}
		return out.data, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := run(ctx, "info", "--format", "{{.ID}}")
	if err != nil {
		return "", nil, "", nil, err
	}
	daemon := strings.TrimSpace(string(raw))
	if daemon == "" || len(daemon) > 200 || strings.ContainsAny(daemon, "\x00\r\n") {
		return "", nil, "", nil, errors.New("invalid local daemon identity")
	}
	return dir, r, daemon, run, nil
}

type ownedCommandOutput struct {
	data     []byte
	overflow bool
}

func (b *ownedCommandOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 1024*1024 {
		b.overflow = true
		return 0, errors.New("bounded Docker output exceeded")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func ownedBuilderAbsent(ctx context.Context, b *ownedBuilderIdentity, run ownedBuilderCommand) (bool, error) {
	raw, err := run(ctx, "container", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return false, err
	}
	if len(raw) > 1024*1024 {
		return false, errors.New("builder inventory exceeds verification limit")
	}
	seen := map[string]bool{}
	present := false
	for _, id := range strings.Fields(string(raw)) {
		if !recoveryContainerID.MatchString(id) || seen[id] {
			return false, errors.New("invalid builder inventory")
		}
		seen[id] = true
		if id == b.ContainerID {
			present = true
		}
	}
	return !present, nil
}

// InterruptPreparationWork confirms only the executor stop. The caller must wait
// for the worker receipt, then restore configuration before reporting cancelled.
// Holding the journal lock serializes removal against worker finalization.
func (d *Docker) InterruptPreparationWork(ctx context.Context, cmd *sdkclient.PollCommand, w *PreparationWork, id string) (bool, error) {
	if cmd == nil || w == nil || cmd.ID != w.CommandID {
		return false, errors.New("interruption belongs to another command")
	}
	_, request, err := d.loadPreparationWork(w, id)
	if err != nil {
		return false, err
	}
	if !request.OwnedBuilder {
		return false, nil
	}
	if cmd.ControlToken == "" || d.Client == nil {
		return false, errors.New("authenticated interruption control required")
	}
	dir, r, daemon, run, err := d.ownedPreparationRuntime(w, id)
	if err != nil {
		return false, err
	}
	// An absent/unbound intent is not authority to adopt a container by name.
	record, err := loadOwnedBuilderRecord(dir, w, id, r.BootID, daemon)
	if err != nil {
		return false, err
	}
	if record.Phase == "intent" || record.Phase == "finished" {
		return false, nil
	}
	err = d.deploymentCheckpoint(ctx, cmd, "preparing")
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, errDeployCancelled) {
		return false, err
	}
	stopped := false
	err = withOwnedBuilderLock(dir, func() error {
		current, err := loadOwnedBuilderRecord(dir, w, id, r.BootID, daemon)
		if err != nil {
			return err
		}
		if current.Phase == "finished" {
			return nil
		}
		if current.Phase == "bound" {
			// Persist the authenticated decision before touching Docker. No tokens go on disk.
			current.Phase = "stopping"
			if err = writeWorkJSON(dir, "builder.json", current); err != nil {
				return err
			}
		}
		if current.Phase != "stopping" && current.Phase != "stopped" {
			return errors.New("builder not bound for interruption")
		}
		absent, err := ownedBuilderAbsent(ctx, current.Identity, run)
		if err != nil {
			return err
		}
		if current.Phase == "stopped" && !absent {
			return errors.New("stopped builder reappeared")
		}
		if !absent {
			if err = removeOwnedBuilder(ctx, current.Identity, w, id, run); err != nil {
				return err
			}
		}
		// A retry after a lost rm response can finish from a complete absence inventory.
		current.Phase = "stopped"
		if err = writeWorkJSON(dir, "builder.json", current); err != nil {
			return err
		}
		stopped = true
		return nil
	})
	return stopped, err
}

func (d *Docker) confirmOwnedPreparationStopped(w *PreparationWork, id string) error {
	return d.confirmOwnedPreparationPhase(w, id, "stopped")
}
func (d *Docker) confirmOwnedPreparationPhase(w *PreparationWork, id, phase string) error {
	dir, r, daemon, run, err := d.ownedPreparationRuntime(w, id)
	if err != nil {
		return err
	}
	record, err := loadOwnedBuilderRecord(dir, w, id, r.BootID, daemon)
	if err != nil {
		return err
	}
	if record.Phase != phase || (record.Profile != "" && record.Profile != "released") {
		return errors.New("worker has no durable executor stop receipt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	absent, err := ownedBuilderAbsent(ctx, record.Identity, run)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("interrupted builder is still present")
	}
	return nil
}

// The private configuration is per-work. Buildx gets a full CID endpoint, never a
// global selected builder or an adoption/recreation permission.
func ownedBuildEnvironment(request *preparationWorkRequest, dir string) []string {
	env := []string{}
	for _, v := range request.Env {
		key, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(key, "BUILDX_") || strings.HasPrefix(key, "BUILDKIT_") || key == "COMPOSE_BAKE" {
			continue
		}
		env = append(env, v)
	}
	return append(env, "BUILDX_CONFIG="+filepath.Join(dir, "buildx"), "COMPOSE_BAKE=false")
}
