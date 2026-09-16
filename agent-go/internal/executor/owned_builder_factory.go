package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Immutable image identity from the audited Docker archive. Preparation is an
// explicit administrative action; ordinary builds never download prerequisites.
const ownedBuilderImage = "sha256:b23a76710ddabde741ffb11d01a8c7e78f58226b330603742e62253133cf9397"

// Docker image stores expose either the archive manifest or its config digest.
// Both identities are fixed from the same audited archive; tags are never used.
const ownedBuilderConfigImage = "sha256:c3e646bcddcda539b2f6831d4b177f0dc339aa45310fb2568c59fdb7f79d751a"

func inspectOwnedBuilderImage(ctx context.Context, run ownedBuilderCommand) ([]byte, error) {
	raw, err := run(ctx, "image", "inspect", ownedBuilderImage)
	if err == nil {
		return raw, nil
	}
	return run(ctx, "image", "inspect", ownedBuilderConfigImage)
}

func verifyOwnedBuilderImage(raw []byte) (string, error) {
	var rows []struct {
		ID           string   `json:"Id"`
		OS           string   `json:"Os"`
		Architecture string   `json:"Architecture"`
		RepoDigests  []string `json:"RepoDigests"`
	}
	if len(raw) > 1024*1024 || json.Unmarshal(raw, &rows) != nil || len(rows) != 1 {
		return "", errors.New("invalid pinned builder image inspection")
	}
	r := rows[0]
	if r.OS != "linux" || r.Architecture != "amd64" || (r.ID != ownedBuilderImage && r.ID != ownedBuilderConfigImage) || !strings.HasPrefix(r.ID, "sha256:") || !workHashPattern.MatchString(strings.TrimPrefix(r.ID, "sha256:")) {
		return "", errors.New("pinned builder image or platform unavailable")
	}
	return r.ID, nil
}

func ownedBuilderPrerequisites(ctx context.Context, run ownedBuilderCommand) (string, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", errors.New("owned builder platform has not been validated")
	}
	raw, err := inspectOwnedBuilderImage(ctx, run)
	if err != nil {
		return "", err
	}
	image, err := verifyOwnedBuilderImage(raw)
	if err != nil {
		return "", err
	}
	if _, err = run(ctx, "buildx", "version"); err != nil {
		return "", err
	}
	raw, err = run(ctx, "compose", "build", "--help")
	if err != nil {
		return "", err
	}
	if len(raw) > 1024*1024 || !slices.Contains(strings.Fields(string(raw)), "--builder") {
		return "", errors.New("Compose does not support an explicit builder")
	}
	return image, nil
}

// Only a new work item can enter creation. Persisting intent before the first
// mutation deliberately makes an ambiguous create non-retryable. Do not adopt a
// container by name after a lost response. Recovery requires a private CID receipt
// and full inspection; creation itself is never replayed.
func createOwnedBuilder(ctx context.Context, dir string, r *ownedBuilderRecord, run ownedBuilderCommand, profile func(context.Context, string, *ownedBuilderRecord, bool) error) (*ownedBuilderRecord, error) {
	if run == nil || profile == nil {
		return nil, errors.New("builder transport unavailable")
	}
	if r == nil {
		return nil, errors.New("missing builder intent")
	}
	r.CIDFile = true
	if err := createOwnedBuilderIntent(dir, r); err != nil {
		return nil, err
	}
	err := withOwnedBuilderLock(dir, func() error {
		current, err := loadOwnedBuilderRecord(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID)
		if err != nil {
			return err
		}
		if current.Phase != "intent" {
			return errors.New("builder creation already attempted")
		}
		if _, err = os.Lstat(filepath.Join(dir, "builder.cid")); !os.IsNotExist(err) {
			return errors.New("builder CID receipt already exists or is inaccessible")
		}
		if err = profile(ctx, dir, current, true); err != nil {
			return err
		}
		name := "impreza-builder-" + r.Work.ID
		args := []string{"container", "create", "--cidfile", filepath.Join(dir, "builder.cid"), "--pull=never", "--name", name,
			"--label", "impreza.builder.work=" + r.Work.ID,
			"--label", "impreza.builder.command=" + r.Work.CommandID,
			"--label", "impreza.builder.deployment=" + r.DeploymentID,
			"--label", "impreza.builder.request=" + r.Work.RequestSHA256,
			"--restart=no", "--network", "bridge", "--ipc", "private",
			"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=" + name,
			"--security-opt", "systempaths=unconfined", "--user", "1000:1000",
			"--volume", "/home/user/.local/share/buildkit", "--pids-limit", "512",
			"--memory", "768m", "--cpus", "1", r.ImageID}
		raw, err := run(ctx, args...)
		if err != nil {
			return errors.New("builder creation uncertain; intent retained")
		}
		cid := strings.TrimSpace(string(raw))
		if !recoveryContainerID.MatchString(cid) {
			return errors.New("invalid builder creation response; intent retained")
		}
		receipt, err := readOwnedBuilderCID(dir)
		if err != nil || receipt != cid {
			return errors.New("builder response and private CID receipt do not match")
		}
		b, err := inspectOwnedBuilderCandidate(ctx, current, cid, run)
		if err != nil {
			return err
		}
		current.Identity, current.Phase = b, "bound"
		if err = writeWorkJSON(dir, "builder.json", current); err != nil {
			return err
		}
		// Bind durably BEFORE start. The same lock excludes cancellation until the
		// start call returns; a stopped executor can never be restarted by this path.
		if _, err = run(ctx, "container", "start", cid); err != nil {
			return errors.New("builder start uncertain; bound journal retained")
		}
		readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for {
			if _, err = run(readyCtx, "container", "exec", cid, "buildctl", "debug", "workers"); err == nil {
				break
			}
			select {
			case <-readyCtx.Done():
				return errors.New("builder readiness uncertain; bound journal retained")
			case <-time.After(200 * time.Millisecond):
			}
		}
		*r = *current
		return nil
	})
	return r, err
}

func ensureOwnedBuilder(ctx context.Context, dir string, w *PreparationWork, request *preparationWorkRequest, daemon string, run ownedBuilderCommand) (*ownedBuilderRecord, error) {
	_, err := os.Lstat(filepath.Join(dir, "builder.json"))
	if err == nil {
		return loadOwnedBuilderRecord(dir, w, request.DeploymentID, request.BootID, daemon)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	image, err := ownedBuilderPrerequisites(ctx, run)
	if err != nil {
		return nil, err
	}
	r := &ownedBuilderRecord{Version: 1, Phase: "intent", Work: *w, DeploymentID: request.DeploymentID, BootID: request.BootID, DaemonID: daemon, ImageID: image, Profile: "planned"}
	return createOwnedBuilder(ctx, dir, r, run, manageOwnedBuilderProfile)
}
