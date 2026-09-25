package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var handoverCommand = regexp.MustCompile(`^cmd_[a-f0-9]{16,32}$`)

// This secret-free identity is safe in the operation journal. Environment
// recovery material lives in a separate private file, removed after API ack.
type DomainHandoverIdentity struct {
	DeploymentID string `json:"deployment_id"`
	Before       string `json:"before"`
	After        string `json:"after"`
}

func (i *DomainHandoverIdentity) Valid() bool {
	return i != nil && recoveryDeploymentID.MatchString(i.DeploymentID) && (i.Before == "" || validHandoverHost(i.Before)) && validHandoverHost(i.After) && i.Before != i.After
}

type domainHandoverRecord struct {
	Version     int                     `json:"version"`
	Command     string                  `json:"command"`
	Identity    DomainHandoverIdentity  `json:"identity"`
	Environment []byte                  `json:"environment"`
	Result      *sdkclient.DeployResult `json:"result,omitempty"`
}

func (d *Docker) handoverPath(command string) (string, error) {
	if !handoverCommand.MatchString(command) {
		return "", errors.New("invalid domain handover command")
	}
	dir := filepath.Join(d.StateDir, "domain-handovers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("invalid domain handover directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	if err := syncRecoveryDirectory(d.StateDir); err != nil {
		return "", err
	}
	return filepath.Join(dir, command+".json"), nil
}

func (d *Docker) saveDomainHandover(r *domainHandoverRecord) error {
	path, err := d.handoverPath(r.Command)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, raw, 0600); err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(path))
}

func (d *Docker) ForgetDomainHandover(command string) error {
	path, err := d.handoverPath(command)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(path))
}

// After a crash, finish a durably verified result or restore the old route.
// Never replay a domain edit using guessed vars or a fresh TLS observation.
func (d *Docker) RecoverDomainHandover(ctx context.Context, command string, identity *DomainHandoverIdentity) (sdkclient.DeployResult, error) {
	if !identity.Valid() || d.Proxy == nil {
		return sdkclient.DeployResult{}, errors.New("invalid domain recovery identity")
	}
	path, err := d.handoverPath(command)
	if err != nil {
		return sdkclient.DeployResult{}, err
	}
	base := sdkclient.DeployResult{CommandID: command, DeploymentID: identity.DeploymentID, Status: "failed", Error: "Interrupted domain handover restored the previous configuration.",
		DomainHandover: &sdkclient.DomainHandoverResult{Before: identity.Before, After: identity.After, Status: "unchanged"}}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return base, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
		return sdkclient.DeployResult{}, errors.New("invalid domain recovery file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return sdkclient.DeployResult{}, err
	}
	var record domainHandoverRecord
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.Command != command || record.Identity != *identity || len(record.Environment) > 1024*1024 {
		return sdkclient.DeployResult{}, errors.New("domain recovery record does not match operation")
	}
	if record.Result != nil {
		r := record.Result
		if r.CommandID != command || r.DeploymentID != identity.DeploymentID || r.DomainHandover == nil || r.DomainHandover.Before != identity.Before || r.DomainHandover.After != identity.After {
			return sdkclient.DeployResult{}, errors.New("domain recovery receipt identity mismatch")
		}
		if r.Status == "success" && r.DomainHandover.Status == "switched" && r.Domain == "https://"+identity.After {
			if err := d.Proxy.FinishDomainHandover(identity.DeploymentID, identity.After); err != nil {
				return sdkclient.DeployResult{}, err
			}
			return *r, nil
		}
		if r.Status == "failed" && r.DomainHandover.Status == "rolled_back" {
			return *r, nil
		}
		return sdkclient.DeployResult{}, errors.New("invalid domain recovery outcome")
	}
	if err := writeAtomic(filepath.Join(d.appDir(identity.DeploymentID), ".env"), record.Environment, 0600); err != nil {
		return sdkclient.DeployResult{}, err
	}
	if err := syncRecoveryDirectory(d.appDir(identity.DeploymentID)); err != nil {
		return sdkclient.DeployResult{}, err
	}
	if err := d.Proxy.RestoreDomainHandover(ctx, identity.DeploymentID, identity.Before, identity.After); err != nil {
		return sdkclient.DeployResult{}, err
	}
	base.DomainHandover.Status = "rolled_back"
	record.Result = &base
	if err := d.saveDomainHandover(&record); err != nil {
		return sdkclient.DeployResult{}, err
	}
	return base, nil
}
