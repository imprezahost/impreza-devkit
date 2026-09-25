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
	"regexp"
)

var (
	journalFenceDeployment = regexp.MustCompile(`^dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})$`)
	journalFenceCutover    = regexp.MustCompile(`^fov_[a-f0-9]{24}$`)
	journalFenceOnion      = regexp.MustCompile(`^[a-z2-7]{56}\.onion$`)
	// Standard base64 of a 32-byte X25519 public key: never secret material.
	journalFenceRecipient = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
)

func validFencePayload(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > 1024 || !json.Valid(raw) {
		return false
	}
	var keys map[string]json.RawMessage
	var p sdkclient.HostFailoverFencePayload
	if json.Unmarshal(raw, &keys) != nil || (len(keys) != 4 && len(keys) != 6) ||
		json.Unmarshal(raw, &p) != nil || !journalFenceDeployment.MatchString(p.DeploymentID) ||
		!journalFenceCutover.MatchString(p.CutoverID) || p.Epoch < 2 ||
		len(p.Hostname) < 4 || len(p.Hostname) > 253 {
		return false
	}
	required := []string{"deployment_id", "hostname", "epoch", "cutover_id"}
	if len(keys) == 6 {
		// onion-transfer-v1 adds the withdrawn address and the target's public recipient.
		if !journalFenceOnion.MatchString(p.Onion) || !journalFenceRecipient.MatchString(p.OnionRecipient) {
			return false
		}
		required = append(required, "onion", "onion_recipient")
	}
	for _, key := range required {
		if _, ok := keys[key]; !ok {
			return false
		}
	}
	return true
}

// Only one outstanding controlled operation is allowed. The receipt can contain
// generated credentials; keep it private and delete only after acknowledgement.
type commandRecord struct {
	DomainHandover  *executor.DomainHandoverIdentity `json:"domain_handover,omitempty"`
	Replacement     *executor.ReplacementWork        `json:"replacement,omitempty"`
	Version         int                              `json:"version"`
	AgentID         string                           `json:"agent_id"`
	ControlPlaneURL string                           `json:"control_plane_url"`
	CommandID       string                           `json:"command_id"`
	Kind            sdkclient.CommandKind            `json:"kind,omitempty"`
	// Only the secret-free failover fence payload may be retained for replay.
	Payload          json.RawMessage               `json:"payload,omitempty"`
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
	if record.Kind == sdkclient.CommandHostFailoverFence {
		if !validFencePayload(record.Payload) {
			return nil, errors.New("invalid saved failover fence payload")
		}
	} else if len(record.Payload) != 0 {
		return nil, errors.New("unexpected operation payload in journal")
	}
	if record.DomainHandover != nil && (record.Kind != sdkclient.CommandUpdateRoutes || !record.DomainHandover.Valid()) {
		return nil, errors.New("invalid domain handover recovery identity")
	}
	if record.Preparation != nil {
		if err := record.Preparation.Validate(); err != nil {
			return nil, err
		}
	}
	if err := validateReplacementRecord(&record); err != nil {
		return nil, err
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
