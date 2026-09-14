package poll

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/executor"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"os"
	"path/filepath"
)

// Only one outstanding controlled operation is allowed. The receipt can contain
// generated credentials; keep it private and delete only after acknowledgement.
type commandRecord struct {
	Version          int                           `json:"version"`
	AgentID          string                        `json:"agent_id"`
	ControlPlaneURL  string                        `json:"control_plane_url"`
	CommandID        string                        `json:"command_id"`
	ControlToken     string                        `json:"control_token"`
	ProgressProtocol string                        `json:"progress_protocol"`
	Step             string                        `json:"step"`
	Preparation      *executor.PreparationRecovery `json:"preparation,omitempty"`
	Result           *sdkclient.DeployResult       `json:"result,omitempty"`
}
type commandJournal struct{ dir string }

func (j *commandJournal) open() (*os.File, error) {
	if err := os.MkdirAll(j.dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(j.dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("operation journal must be a real directory")
	}
	if err := os.Chmod(j.dir, 0700); err != nil {
		return nil, err
	}
	if err := syncJournalDirectory(filepath.Dir(j.dir)); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(j.dir, "lock")
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("operation lock must be a regular file")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockJournal(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("another agent owns this operation journal: %w", err)
	}
	return f, nil
}
func (j *commandJournal) load() (*commandRecord, error) {
	name := filepath.Join(j.dir, "pending.json")
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, errors.New("invalid operation journal file")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 4<<20))
	dec.DisallowUnknownFields()
	var record commandRecord
	if err := dec.Decode(&record); err != nil {
		return nil, fmt.Errorf("invalid operation journal: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("operation journal has trailing data")
	}
	if record.Version != 1 || record.AgentID == "" || record.ControlPlaneURL == "" || record.CommandID == "" || record.ControlToken == "" || record.ProgressProtocol != sdkclient.DeploymentProgressProtocol {
		return nil, errors.New("operation journal identity or protocol is invalid")
	}
	if record.Preparation != nil {
		if err := record.Preparation.Validate(); err != nil {
			return nil, err
		}
	}
	if r := record.Result; r != nil {
		if r.CommandID != record.CommandID || r.ControlToken != record.ControlToken {
			return nil, errors.New("saved result does not match the operation")
		}
		switch r.Status {
		case "success", "failed", "cancelled", "timeout", "partial":
		default:
			return nil, errors.New("saved result has invalid status")
		}
	}
	return &record, nil
}
func (j *commandJournal) save(record *commandRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("operation receipt exceeds journal size limit")
	}
	f, err := os.CreateTemp(j.dir, ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(j.dir, "pending.json")); err != nil {
		return err
	}
	return syncJournalDirectory(j.dir)
}
func (j *commandJournal) clear() error {
	if err := os.Remove(filepath.Join(j.dir, "pending.json")); err != nil {
		return err
	}
	return syncJournalDirectory(j.dir)
}
