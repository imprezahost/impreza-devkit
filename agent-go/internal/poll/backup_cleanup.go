package poll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var backupCleanupID = regexp.MustCompile(`^bkpjob_(?:[a-f0-9]{16}|[a-f0-9]{24})$`)

// errBackupCleanupInventoryFull is a soft cap, not a storage failure: an already
// acknowledged result must still be released, or a full inventory would hold
// the command queue.
var errBackupCleanupInventoryFull = errors.New("backup cleanup inventory requires reconciliation")

type backupCleanupRecord struct {
	Version         int       `json:"version"`
	AgentID         string    `json:"agent_id"`
	ControlPlaneURL string    `json:"control_plane_url"`
	CommandID       string    `json:"command_id"`
	DeploymentID    string    `json:"deployment_id"`
	Status          string    `json:"status"`
	Attempts        int       `json:"attempts"`
	NextAttempt     time.Time `json:"next_attempt"`
}

func (p *Poller) cleanupDirectory() (string, error) {
	if p.journal == nil {
		return "", errors.New("backup cleanup requires durable operation storage")
	}
	dir := filepath.Join(p.journal.dir, "acknowledged-backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	i, err := os.Lstat(dir)
	if err != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("unsafe backup cleanup directory")
	}
	return dir, nil
}
func cleanupName(r backupCleanupRecord) string {
	h := sha256.Sum256([]byte(r.AgentID + "\x00" + r.ControlPlaneURL + "\x00" + r.CommandID))
	return hex.EncodeToString(h[:]) + ".json"
}
func saveBackupCleanup(dir string, r backupCleanupRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".cleanup-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	name := filepath.Join(dir, cleanupName(r))
	if i, e := os.Lstat(name); e == nil && !i.Mode().IsRegular() {
		return errors.New("unsafe backup cleanup file")
	}
	if err = os.Rename(f.Name(), name); err != nil {
		return err
	}
	return syncJournalDirectory(dir)
}
func (p *Poller) queueAcknowledgedBackup(kind sdkclient.CommandKind, result sdkclient.DeployResult) error {
	if kind != sdkclient.CommandDeploy || !backupCleanupID.MatchString(result.DeploymentID) || (result.Status != "success" && result.Status != "failed" && result.Status != "cancelled") {
		return nil
	}
	dir, err := p.cleanupDirectory()
	if err != nil {
		return err
	}
	r := backupCleanupRecord{Version: 1, AgentID: p.cfg.AgentID, ControlPlaneURL: p.cfg.ControlPlaneURL, CommandID: result.CommandID, DeploymentID: result.DeploymentID, Status: result.Status}
	// Repeated ACK after a journal-clear failure must not reset the retry budget.
	name := filepath.Join(dir, cleanupName(r))
	if i, e := os.Lstat(name); e == nil {
		if !i.Mode().IsRegular() || i.Size() > 8192 {
			return errors.New("unsafe existing cleanup record")
		}
		raw, e := os.ReadFile(name)
		if e != nil {
			return e
		}
		var existing backupCleanupRecord
		if json.Unmarshal(raw, &existing) != nil || existing.Version != 1 || existing.AgentID != r.AgentID || existing.ControlPlaneURL != r.ControlPlaneURL || existing.CommandID != r.CommandID || existing.DeploymentID != r.DeploymentID || existing.Status != r.Status || existing.Attempts < 0 || existing.Attempts > 3 {
			return errors.New("cleanup receipt identity changed")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) >= 1024 {
		return errBackupCleanupInventoryFull
	}
	return saveBackupCleanup(dir, r)
}

// One bounded attempt per minute, at most three per accepted receipt. Exhausted
// records remain for support; cleanup can never cause a result to be resent.
func (p *Poller) reconcileAcknowledgedBackups(ctx context.Context) {
	if p.journal == nil || time.Now().Before(p.nextBackupCleanup) {
		return
	}
	p.nextBackupCleanup = time.Now().Add(time.Minute)
	dir, err := p.cleanupDirectory()
	if err != nil {
		p.log.Warn("backup cleanup storage requires reconciliation", "err", err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := filepath.Join(dir, entry.Name())
		i, e := os.Lstat(name)
		if e != nil || !i.Mode().IsRegular() || i.Size() > 8192 {
			continue
		}
		f, e := os.Open(name)
		if e != nil {
			continue
		}
		dec := json.NewDecoder(io.LimitReader(f, 8192))
		dec.DisallowUnknownFields()
		var r backupCleanupRecord
		e = dec.Decode(&r)
		var tail any
		extra := dec.Decode(&tail)
		f.Close()
		if e != nil || extra != io.EOF || r.Version != 1 || r.AgentID != p.cfg.AgentID || r.ControlPlaneURL != p.cfg.ControlPlaneURL || r.CommandID == "" || !backupCleanupID.MatchString(r.DeploymentID) || entry.Name() != cleanupName(r) || r.Attempts < 0 || r.Attempts >= 3 || time.Now().Before(r.NextAttempt) || (r.Status != "success" && r.Status != "failed" && r.Status != "cancelled") {
			continue
		}
		r.Attempts++
		r.NextAttempt = time.Now().Add(time.Minute)
		if e = saveBackupCleanup(dir, r); e != nil {
			p.log.Warn("backup cleanup attempt could not be recorded", "err", e)
			return
		}
		work, cancel := context.WithTimeout(ctx, 10*time.Second)
		e = p.cleanupAcknowledgedResult(work, sdkclient.CommandDeploy, sdkclient.DeployResult{CommandID: r.CommandID, DeploymentID: r.DeploymentID, Status: r.Status})
		cancel()
		if e == nil {
			if e = os.Remove(name); e == nil {
				e = syncJournalDirectory(dir)
			}
		}
		if e != nil {
			p.log.Warn("acknowledged backup requires cleanup reconciliation", "command_id", r.CommandID, "attempts", r.Attempts, "err", e)
		}
		return
	}
}
