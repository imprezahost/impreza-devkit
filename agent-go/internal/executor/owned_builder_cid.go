package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
)

// Docker creates this file in the private work directory. A missing, partial or
// unsafe file is not evidence of absence and must never authorize name adoption.
func readOwnedBuilderCID(dir string) (string, error) {
	if err := realWorkDirectory(dir); err != nil {
		return "", err
	}
	f, err := openOwnedBuilderCID(filepath.Join(dir, "builder.cid"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 64 {
		return "", errors.New("invalid private builder CID receipt")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", err
	}
	cid := string(raw)
	if !recoveryContainerID.MatchString(cid) {
		return "", errors.New("invalid full builder CID")
	}
	// Persist before binding. A crash before a valid file is available stays manual.
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = syncRecoveryDirectory(dir); err != nil {
		return "", err
	}
	return cid, nil
}

func inspectOwnedBuilderCandidate(ctx context.Context, r *ownedBuilderRecord, cid string, run ownedBuilderCommand) (*ownedBuilderIdentity, error) {
	if r == nil || !recoveryContainerID.MatchString(cid) || run == nil {
		return nil, errors.New("invalid builder candidate")
	}
	raw, err := run(ctx, "container", "inspect", cid)
	if err != nil {
		return nil, err
	}
	var rows []ownedBuilderInspection
	if len(raw) > 1024*1024 || json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || len(rows[0].Mounts) != 1 {
		return nil, errors.New("invalid new builder inspection")
	}
	b := &ownedBuilderIdentity{Version: 1, WorkID: r.Work.ID, CommandID: r.Work.CommandID, DeploymentID: r.DeploymentID, RequestSHA256: r.Work.RequestSHA256, ContainerID: cid, ImageID: r.ImageID, StateVolume: rows[0].Mounts[0].Name}
	if err = b.validate(&r.Work, r.DeploymentID); err != nil {
		return nil, err
	}
	if err = b.verifyInspection(raw); err != nil {
		return nil, err
	}
	return b, nil
}
