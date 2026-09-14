package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// PreparationRecovery is private journal data, not a command to replay. A ready
// checkpoint proves all preceding external operations returned successfully.
// Busy/blocked/replacing checkpoints never authorize automatic reconciliation.
type PreparationRecovery struct {
	Version      int               `json:"version"`
	Phase        string            `json:"phase"`
	DeploymentID string            `json:"deployment_id,omitempty"`
	Files        []PreparationFile `json:"files,omitempty"`
	Containers   []string          `json:"containers,omitempty"`
}
type PreparationFile struct {
	Name   string `json:"name"`
	Data   []byte `json:"data,omitempty"`
	Mode   uint32 `json:"mode"`
	Exists bool   `json:"exists"`
}

var recoveryDeploymentID = regexp.MustCompile("^dpl_[A-Za-z0-9_-]{1,120}$")
var recoveryContainerID = regexp.MustCompile("^[a-f0-9]{64}$")

func (r *PreparationRecovery) Validate() error {
	if r == nil || r.Version != 1 {
		return errors.New("invalid preparation recovery version")
	}
	if r.Phase == "unstarted" && r.DeploymentID == "" && len(r.Files) == 0 && len(r.Containers) == 0 {
		return nil
	}
	if !slices.Contains([]string{"ready", "busy", "blocked", "replacing"}, r.Phase) || !recoveryDeploymentID.MatchString(r.DeploymentID) || len(r.Files) != 3 {
		return errors.New("invalid preparation checkpoint")
	}
	for i, name := range []string{"compose.yaml", ".env", "startup.json"} {
		f := r.Files[i]
		if f.Name != name || f.Mode & ^uint32(0777) != 0 || (!f.Exists && (len(f.Data) != 0 || f.Mode != 0)) {
			return errors.New("invalid preparation configuration snapshot")
		}
	}
	seen := map[string]bool{}
	for _, id := range r.Containers {
		if !recoveryContainerID.MatchString(id) || seen[id] {
			return errors.New("invalid preparation container identity")
		}
		seen[id] = true
	}
	return nil
}
func (r *PreparationRecovery) Recoverable() bool {
	return r != nil && r.Validate() == nil && (r.Phase == "unstarted" || r.Phase == "ready")
}

// Lstat every component below StateDir. Never follow an app-directory or config
// symlink while capturing or restoring another command's private configuration.
func (d *Docker) recoveryPath(id string) (string, error) {
	if !recoveryDeploymentID.MatchString(id) {
		return "", errors.New("invalid recovery deployment identity")
	}
	for _, path := range []string{d.StateDir, filepath.Join(d.StateDir, "apps"), d.appDir(id)} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("recovery path must contain only real directories")
		}
	}
	return d.appDir(id), nil
}
func (d *Docker) recoveryContainers(ctx context.Context, id string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancel()
	raw, err := d.dockerCmd(ctx, "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project="+strings.ToLower(id)).Output()
	if err != nil {
		return nil, fmt.Errorf("verify preparation container identities: %w", err)
	}
	ids := strings.Fields(string(raw))
	slices.Sort(ids)
	for _, id := range ids {
		if !recoveryContainerID.MatchString(id) {
			return nil, errors.New("invalid Docker container identity")
		}
	}
	return ids, nil
}
func (d *Docker) capturePreparation(ctx context.Context, id string, files deployConfigSnapshot) (*PreparationRecovery, error) {
	if _, err := d.recoveryPath(id); err != nil {
		return nil, err
	}
	ids, err := d.recoveryContainers(ctx, id)
	if err != nil {
		return nil, err
	}
	r := &PreparationRecovery{Version: 1, Phase: "busy", DeploymentID: id, Containers: ids}
	for _, f := range files {
		r.Files = append(r.Files, PreparationFile{Name: f.name, Data: f.data, Mode: uint32(f.mode), Exists: f.exists})
	}
	return r, r.Validate()
}

// ReconcilePreparation only restores the three configuration files. It never
// runs Compose, replays payloads, replaces containers, changes volumes or reports
// runtime health. Repetition after a second crash is safe.
func (d *Docker) ReconcilePreparation(ctx context.Context, r *PreparationRecovery) error {
	if !r.Recoverable() {
		return errors.New("preparation has no completed safe checkpoint")
	}
	if r.Phase == "unstarted" {
		return nil
	}
	dir, err := d.recoveryPath(r.DeploymentID)
	if err != nil {
		return err
	}
	ids, err := d.recoveryContainers(ctx, r.DeploymentID)
	if err != nil {
		return err
	}
	if !slices.Equal(ids, r.Containers) {
		return errors.New("containers changed since preparation; manual reconciliation required")
	}
	for _, f := range r.Files {
		info, err := os.Lstat(filepath.Join(dir, f.Name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && !info.Mode().IsRegular() {
			return errors.New("configuration is not a regular file; manual reconciliation required")
		}
	}
	if _, err := os.Lstat(dir); err != nil {
		return err
	}
	snapshot := deployConfigSnapshot{}
	for _, f := range r.Files {
		snapshot = append(snapshot, deployConfigFile{name: f.Name, data: f.Data, mode: os.FileMode(f.Mode), exists: f.Exists})
	}
	if err := snapshot.restore(dir); err != nil {
		return err
	}
	// Verify exact bytes/existence/modes after the durable restore.
	restored, err := captureDeployConfig(dir, true)
	if err != nil {
		return err
	}
	for i, f := range restored {
		expected := r.Files[i]
		if f.exists != expected.Exists || !slices.Equal(f.data, expected.Data) || (f.exists && uint32(f.mode) != expected.Mode) {
			return errors.New("restored configuration verification failed")
		}
	}
	ids, err = d.recoveryContainers(ctx, r.DeploymentID)
	if err != nil {
		return err
	}
	if !slices.Equal(ids, r.Containers) {
		return errors.New("containers changed during restoration; manual reconciliation required")
	}
	return nil
}
