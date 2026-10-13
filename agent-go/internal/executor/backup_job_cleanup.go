package executor

import (
	"context"
	"encoding/json"
	"errors"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A transport deployment can report startup while its backup is still running.
// Never stop such work: the portal's completion callback queues its uninstall.
// Finished jobs keep their result in the poll/restore journal, outside apps/.
func (d *Docker) cleanupFinishedBackupJob(ctx context.Context, id string) error {
	if !backupJobPattern.MatchString(id) {
		return nil
	}
	dir := d.appDir(id)
	for _, p := range []string{filepath.Join(d.StateDir, "apps"), dir} {
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("backup job directory could not be verified")
		}
	}
	project, err := composeProject(dir)
	if err != nil || project != id {
		return errors.New("backup job project could not be verified")
	}
	record, err := d.loadQuiesceRecord(id)
	if err != nil {
		return errors.New("backup job recovery state could not be verified")
	}
	if record != nil {
		if _, ok := d.quiesceReadResult(id, record.CommandID); !ok {
			return errors.New("backup job terminal receipt could not be verified")
		}
	}
	if record != nil && record.Phase == "quiesce_orphan" {
		return nil
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	label := "com.docker.compose.project=" + id
	out, err := d.dockerCmd(work, "ps", "-aq", "--filter", "label="+label).Output()
	if err != nil {
		return errors.New("backup job containers could not be verified")
	}
	ids := strings.Fields(string(out))
	if len(ids) > 0 {
		out, err = d.dockerCmd(work, append([]string{"inspect"}, ids...)...).Output()
		var states []struct {
			ID    string `json:"Id"`
			State struct {
				Status     string `json:"Status"`
				Running    bool   `json:"Running"`
				Restarting bool   `json:"Restarting"`
				Paused     bool   `json:"Paused"`
			} `json:"State"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if err != nil || json.Unmarshal(out, &states) != nil || len(states) != len(ids) {
			return errors.New("backup job containers could not be verified")
		}
		for _, s := range states {
			if s.Config.Labels["com.docker.compose.project"] != id {
				return errors.New("backup job ownership could not be verified")
			}
			if s.State.Status != "exited" || s.State.Running || s.State.Restarting || s.State.Paused {
				return nil
			}
		}
		// No force: a container started concurrently must make removal fail.
		if _, err = d.dockerCmd(work, append([]string{"rm", "-v"}, ids...)...).CombinedOutput(); err != nil {
			return errors.New("backup job container cleanup is pending")
		}
	}
	out, err = d.dockerCmd(work, "ps", "-aq", "--filter", "label="+label).Output()
	if err != nil || len(strings.Fields(string(out))) != 0 {
		return errors.New("backup job container cleanup is pending")
	}
	out, err = d.dockerCmd(work, "network", "ls", "-q", "--filter", "label="+label).Output()
	if err != nil {
		return errors.New("backup job network cleanup is pending")
	}
	for _, network := range strings.Fields(string(out)) {
		if _, err = d.dockerCmd(work, "network", "rm", network).CombinedOutput(); err != nil {
			return errors.New("backup job network cleanup is pending")
		}
	}
	if err = os.RemoveAll(dir); err != nil {
		return errors.New("backup job directory cleanup is pending")
	}
	return nil
}

// CleanupAcknowledgedResult runs only after the control plane has accepted the
// receipt. Running transport jobs remain owned by their completion callback.
func (d *Docker) CleanupAcknowledgedResult(ctx context.Context, kind sdkclient.CommandKind, result sdkclient.DeployResult) error {
	if kind != sdkclient.CommandDeploy || (result.Status != "success" && result.Status != "failed" && result.Status != "cancelled") {
		return nil
	}
	return d.cleanupFinishedBackupJob(ctx, result.DeploymentID)
}
