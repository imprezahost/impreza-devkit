package executor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// This journal is a prerequisite for an owned executor, not a cancellation grant.
// No existing deployment uses it until the execution path is explicitly integrated.
type ownedBuilderRecord struct {
	CIDFile        bool                  `json:"cid_file,omitempty"`
	Version        int                   `json:"version"`
	Phase          string                `json:"phase"`
	Work           PreparationWork       `json:"work"`
	DeploymentID   string                `json:"deployment_id"`
	BootID         string                `json:"boot_id"`
	DaemonID       string                `json:"daemon_id"`
	ImageID        string                `json:"image_id"`
	Identity       *ownedBuilderIdentity `json:"identity,omitempty"`
	RecoveryBootID string                `json:"recovery_boot_id,omitempty"`
	Profile        string                `json:"profile,omitempty"`
}

func (r *ownedBuilderRecord) validate(w *PreparationWork, deployment, boot, daemon string) error {
	if r == nil || w == nil || w.validate() != nil || r.Version != 1 || r.Work != *w || w.Step != "build" || !recoveryDeploymentID.MatchString(deployment) || r.DeploymentID != deployment || !bootIDPattern.MatchString(boot) || r.BootID != boot || daemon == "" || len(daemon) > 200 || strings.ContainsAny(daemon, "\x00\r\n") || r.DaemonID != daemon || !strings.HasPrefix(r.ImageID, "sha256:") || !workHashPattern.MatchString(strings.TrimPrefix(r.ImageID, "sha256:")) {
		return errors.New("owned builder journal identity mismatch")
	}
	if !slices.Contains([]string{"intent", "bound", "stopping", "stopped", "finished", "recovering", "recovered"}, r.Phase) {
		return errors.New("invalid owned builder journal phase")
	}
	if !slices.Contains([]string{"", "planned", "loaded", "released"}, r.Profile) ||
		(r.Profile == "planned" && r.Phase != "intent") ||
		(r.Profile == "released" && r.Phase != "finished" && r.Phase != "stopped" && r.Phase != "recovering" && r.Phase != "recovered") {
		return errors.New("invalid owned builder profile state")
	}
	if (r.Phase == "recovering" || r.Phase == "recovered") != (r.RecoveryBootID != "") || (r.RecoveryBootID != "" && !bootIDPattern.MatchString(r.RecoveryBootID)) {
		return errors.New("invalid builder recovery boot")
	}
	if r.Phase == "intent" {
		if r.Identity != nil {
			return errors.New("unbound intent contains a container")
		}
		return nil
	}
	if err := r.Identity.validate(w, deployment); err != nil {
		return err
	}
	if r.Identity.ImageID != r.ImageID {
		return errors.New("builder image differs from original intent")
	}
	return nil
}

// The lock serializes worker/control access across processes. An intent must be
// persisted before Docker create; a launch with an uncertain result is never retried.
func withOwnedBuilderLock(dir string, action func() error) error {
	if err := realWorkDirectory(dir); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("builder journal directory is not private")
	}
	path := filepath.Join(dir, "builder.lock")
	if info, err = os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("invalid builder lock file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := openOwnedBuilderLock(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = lockOwnedBuilderFile(file); err != nil {
		return err
	}
	return action()
}

func loadOwnedBuilderRecord(dir string, w *PreparationWork, deployment, boot, daemon string) (*ownedBuilderRecord, error) {
	var r ownedBuilderRecord
	if _, err := readWorkJSON(filepath.Join(dir, "builder.json"), &r); err != nil {
		return nil, err
	}
	if err := r.validate(w, deployment, boot, daemon); err != nil {
		return nil, err
	}
	return &r, nil
}

func createOwnedBuilderIntent(dir string, r *ownedBuilderRecord) error {
	if r == nil {
		return errors.New("missing builder intent")
	}
	if err := r.validate(&r.Work, r.DeploymentID, r.BootID, r.DaemonID); err != nil {
		return err
	}
	if r.Phase != "intent" {
		return errors.New("builder must begin with an unbound intent")
	}
	return withOwnedBuilderLock(dir, func() error {
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, "builder.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return syncRecoveryDirectory(dir)
	})
}

// Only adjacent state transitions are legal. The caller must verify Docker identity
// when binding and persist an authenticated stop intent before any destructive call.
// `stopped` may only follow a successful removal AND verified absence.
func transitionOwnedBuilder(dir string, w *PreparationWork, deployment, boot, daemon, from, to string, b *ownedBuilderIdentity) error {
	allowed := map[string]string{"intent": "bound", "bound": "stopping", "stopping": "stopped"}
	if allowed[from] != to {
		return errors.New("invalid builder transition")
	}
	return withOwnedBuilderLock(dir, func() error {
		r, err := loadOwnedBuilderRecord(dir, w, deployment, boot, daemon)
		if err != nil {
			return err
		}
		if r.Phase != from {
			return errors.New("builder state changed; do not repeat the operation")
		}
		if from == "intent" {
			if b == nil {
				return errors.New("binding requires a verified container identity")
			}
			copyIdentity := *b
			r.Identity = &copyIdentity
		} else if b != nil {
			return errors.New("bound builder identity is immutable")
		}
		r.Phase = to
		if err = r.validate(w, deployment, boot, daemon); err != nil {
			return err
		}
		return writeWorkJSON(dir, "builder.json", r)
	})
}
