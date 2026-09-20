package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// A process can die between two fragment writes. Keep both old fragments in
// private durable storage and block every unrelated route mutation until the
// move is verified or its rollback is proven. Startup never guesses a route.
type routingSwitchRecord struct {
	Version       int    `json:"version"`
	Hostname      string `json:"hostname"`
	Source        string `json:"source"`
	Target        string `json:"target"`
	SourceBefore  []byte `json:"source_before"`
	TargetBefore  []byte `json:"target_before"`
	TargetExisted bool   `json:"target_existed"`
}

func (c *Caddy) switchRecordPath() string { return filepath.Join(c.StateDir, "routing-switch.json") }
func (c *Caddy) guardRoutingSwitch() error {
	_, err := os.Lstat(c.switchRecordPath())
	if os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("%w: reconcile the private routing-switch.json before changing proxy routes", ErrRoutingRecoveryRequired)
}
func (c *Caddy) beginRoutingSwitch(r routingSwitchRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(c.switchRecordPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("%w: cannot persist routing recovery record", ErrRoutingRecoveryRequired)
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return syncRoutingDir(c.StateDir)
}

// CompleteHostnameSwitch is called only after the route probe or rollback
// reload succeeds. Losing the API acknowledgement is handled by the poller's
// existing durable result journal; this record protects local fragment writes.
func (c *Caddy) CompleteHostnameSwitch(hostname, source, target string) error {
	info, err := os.Lstat(c.switchRecordPath())
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		return ErrRoutingRecoveryRequired
	}
	raw, err := os.ReadFile(c.switchRecordPath())
	if err != nil {
		return err
	}
	var r routingSwitchRecord
	if json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.Hostname != hostname || r.Source != source || r.Target != target {
		return ErrRoutingRecoveryRequired
	}
	if err = os.Remove(c.switchRecordPath()); err != nil {
		return err
	}
	return syncRoutingDir(c.StateDir)
}
func syncRoutingDir(dir string) error {
	// Directory fsync is unsupported on Windows; the production agent is Linux.
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
