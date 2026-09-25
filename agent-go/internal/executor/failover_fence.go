package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var (
	failoverDeploymentID = regexp.MustCompile(`^dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})$`)
	failoverTransportID  = regexp.MustCompile(`^(?:bkpjob|rstjob|tskjob|rdjob|clijob|pitrjob)_[a-f0-9]{16}$`)
	failoverCutoverID    = regexp.MustCompile(`^fov_[a-f0-9]{24}$`)
	failoverHostname     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
	failoverOnion        = regexp.MustCompile(`^[a-z2-7]{56}\.onion$`)
)

func (d *Docker) checkCommandFences(kind sdkclient.CommandKind, id string, dependencies []string) error {
	ids := []string{id}
	if failoverTransportID.MatchString(id) {
		// A backup/CLI job has no app tombstone of its own. The authenticated
		// control plane derives its real dependencies from private job rows.
		// Missing identities must not let a stale job bypass a host fence.
		if kind != sdkclient.CommandDeploy || len(dependencies) == 0 || len(dependencies) > 256 {
			return errors.New("Internal job fence dependencies cannot be verified; prepare a new operation.")
		}
		ids = dependencies
	}
	for _, app := range ids {
		fence, err := d.readFailoverFence(app)
		if err != nil {
			return errors.New("Failover fence state cannot be verified; refusing to change the deployment.")
		}
		if fence != nil {
			return errors.New("Deployment is fenced after a cross-host failover; prepare a new reviewed operation.")
		}
	}
	return nil
}

// This tombstone lives outside apps/<id>: an uninstall may erase that tree,
// but must not make a stale deploy or route job able to re-serve the old host.
type failoverFence struct {
	Version      int    `json:"version"`
	DeploymentID string `json:"deployment_id"`
	Hostname     string `json:"hostname"`
	Epoch        uint64 `json:"epoch"`
	CutoverID    string `json:"cutover_id"`
	// Onion is the identity this fence withdrew; startup keeps it unpublished.
	Onion string `json:"onion,omitempty"`
}

func blocksWhileFenced(kind sdkclient.CommandKind) bool {
	switch kind {
	case sdkclient.CommandDeploy, sdkclient.CommandRollback, sdkclient.CommandRestart,
		sdkclient.CommandUpdateRoutes, sdkclient.CommandTrafficSwitch,
		sdkclient.CommandOnionAuthUpdate, sdkclient.CommandOnionProfileUpdate,
		sdkclient.CommandOnionKeyExport, sdkclient.CommandOnionRotate,
		sdkclient.CommandOnionTransferRecipient:
		return true
	default:
		return false
	}
}

func validFailoverHostname(host string) bool {
	if len(host) < 4 || len(host) > 253 || strings.Contains(host, "..") || !failoverHostname.MatchString(host) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return strings.Contains(host, ".")
}

func (d *Docker) failoverFencePath(deploymentID string) (string, error) {
	if !failoverDeploymentID.MatchString(deploymentID) {
		return "", errors.New("invalid deployment identity")
	}
	return filepath.Join(d.StateDir, "failover-fences", deploymentID+".json"), nil
}

func (d *Docker) readFailoverFence(deploymentID string) (*failoverFence, error) {
	path, err := d.failoverFencePath(deploymentID)
	if err != nil {
		return nil, err
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !dir.IsDir() || (runtime.GOOS != "windows" && dir.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("unsafe failover fence directory")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("unsafe failover fence file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("failover fence changed while being read")
	}
	raw := make([]byte, info.Size())
	if _, err := io.ReadFull(file, raw); err != nil {
		return nil, err
	}
	var f failoverFence
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || f.DeploymentID != deploymentID || f.Epoch < 2 ||
		!validFailoverHostname(f.Hostname) || !failoverCutoverID.MatchString(f.CutoverID) ||
		(f.Onion != "" && !failoverOnion.MatchString(f.Onion)) {
		return nil, errors.New("invalid failover fence state")
	}
	return &f, nil
}

func (d *Docker) writeFailoverFence(f failoverFence) error {
	path, err := d.failoverFencePath(f.DeploymentID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return errors.New("unsafe failover fence directory")
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, raw, 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		fd, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer fd.Close()
		if err := fd.Sync(); err != nil {
			return err
		}
	}
	return syncRecoveryDirectory(d.StateDir)
}

// failoverFailure names the application a failover command was for. The
// control plane identifies the job by command and agent; an explicit identity
// keeps a controlled result acknowledgeable instead of retried forever.
func failoverFailure(commandID, deploymentID, msg string) sdkclient.DeployResult {
	result := failResult(commandID, msg)
	if failoverDeploymentID.MatchString(deploymentID) {
		result.DeploymentID = deploymentID
	}
	return result
}

// hostFailoverFence is intentionally idempotent. Persist the tombstone first:
// a crash can leave the operation incomplete, but can never make a later
// command restart containers or routes while the control plane retries it.
func (d *Docker) hostFailoverFence(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.HostFailoverFencePayload
	if err := cmd.As(&p); err != nil || !failoverDeploymentID.MatchString(p.DeploymentID) ||
		!validFailoverHostname(p.Hostname) || p.Epoch < 2 || !failoverCutoverID.MatchString(p.CutoverID) {
		return failoverFailure(cmd.ID, p.DeploymentID, "Invalid host failover fence request.")
	}
	// onion-transfer-v1: both fields or neither. The recipient is a public key.
	var recipient *[32]byte
	if p.Onion != "" || p.OnionRecipient != "" {
		raw, err := base64.StdEncoding.DecodeString(p.OnionRecipient)
		if !failoverOnion.MatchString(p.Onion) || err != nil || len(raw) != 32 ||
			base64.StdEncoding.EncodeToString(raw) != p.OnionRecipient || d.Tor == nil {
			return failoverFailure(cmd.ID, p.DeploymentID, "Invalid onion transfer in the host failover fence.")
		}
		recipient = new([32]byte)
		copy(recipient[:], raw)
	}
	old, err := d.readFailoverFence(p.DeploymentID)
	if err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "Existing failover fence cannot be verified.")
	}
	if old != nil && (old.Epoch > p.Epoch || (old.Epoch == p.Epoch &&
		(old.CutoverID != p.CutoverID || old.Hostname != p.Hostname || old.Onion != p.Onion))) {
		return failoverFailure(cmd.ID, p.DeploymentID, "A newer or different failover fence already exists.")
	}
	appDir := d.appDir(p.DeploymentID)
	if !exists(appDir) || d.Proxy == nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "The old application and proxy must be available to confirm fencing.")
	}
	if err := d.writeFailoverFence(failoverFence{1, p.DeploymentID, p.Hostname, p.Epoch, p.CutoverID, p.Onion}); err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "Failover fence could not be persisted.")
	}
	// Disable restart durably before acknowledging any fence. Compose stop
	// alone permits restart:always containers to reappear on a daemon reboot.
	stopCtx, cancelStop := context.WithTimeout(ctx, composeRestartTimeout)
	err = d.stopFencedContainers(stopCtx, p.DeploymentID)
	cancelStop()
	if err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "Old container shutdown could not be verified; fencing is incomplete.")
	}
	routeCtx, cancelRoutes := context.WithTimeout(ctx, composeQueryTimeout)
	err = d.Proxy.ApplyDeploymentRoutes(routeCtx, p.DeploymentID, nil)
	cancelRoutes()
	if err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, fmt.Sprintf("Old routes could not be removed: %v", err))
	}
	proof := &sdkclient.HostFailoverFenceResult{
		DeploymentID: p.DeploymentID, Hostname: p.Hostname, Epoch: p.Epoch,
		CutoverID: p.CutoverID, RoutesRemoved: true, ContainersStopped: true,
	}
	if recipient != nil {
		// Stop publishing before sealing: two hosts must never announce the
		// same identity. The parked copy stays until the purge after cutover.
		torCtx, cancelTor := context.WithTimeout(ctx, composeQueryTimeout)
		err = d.Tor.WithdrawHiddenService(torCtx, p.DeploymentID, p.CutoverID, p.Onion)
		cancelTor()
		if err != nil {
			return failoverFailure(cmd.ID, p.DeploymentID, "Old onion service could not be withdrawn; fencing is incomplete.")
		}
		sealed, err := d.Tor.ExportWithdrawnOnionKey(p.DeploymentID, p.CutoverID, p.Onion, recipient)
		if err != nil {
			return failoverFailure(cmd.ID, p.DeploymentID, "Withdrawn onion identity could not be sealed for the standby.")
		}
		proof.Onion = p.Onion
		proof.OnionWithdrawn = true
		proof.OnionSealed = base64.StdEncoding.EncodeToString(sealed)
	}
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID,
		HostFailoverFence: proof}
}

// hostFailoverRelease lifts exactly the fence one verified cutover left, so a
// reviewed failback can restore a standby copy here. It never starts the old
// writer: containers are re-verified stopped with restart disabled, a fenced
// onion stays withdrawn, and the deployment has no route. Idempotent: with no
// tombstone left the end state already holds.
func (d *Docker) hostFailoverRelease(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.HostFailoverReleasePayload
	if err := cmd.As(&p); err != nil || !failoverDeploymentID.MatchString(p.DeploymentID) ||
		!validFailoverHostname(p.Hostname) || p.Epoch < 2 || !failoverCutoverID.MatchString(p.CutoverID) {
		return failoverFailure(cmd.ID, p.DeploymentID, "Invalid host failover release request.")
	}
	fence, err := d.readFailoverFence(p.DeploymentID)
	if err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "Existing failover fence cannot be verified; nothing was released.")
	}
	proof := &sdkclient.HostFailoverReleaseResult{DeploymentID: p.DeploymentID, Hostname: p.Hostname,
		Epoch: p.Epoch, CutoverID: p.CutoverID, Released: true, ContainersStopped: true}
	if fence == nil {
		return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID,
			HostFailoverRelease: proof}
	}
	if fence.CutoverID != p.CutoverID || fence.Epoch != p.Epoch || fence.Hostname != p.Hostname {
		return failoverFailure(cmd.ID, p.DeploymentID, "A different failover fence is active; nothing was released.")
	}
	stopCtx, cancelStop := context.WithTimeout(ctx, composeRestartTimeout)
	err = d.stopFencedContainers(stopCtx, p.DeploymentID)
	cancelStop()
	if err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "Old containers could not be verified stopped; the fence was kept.")
	}
	if fence.Onion != "" && d.Tor != nil {
		torCtx, cancelTor := context.WithTimeout(ctx, composeQueryTimeout)
		err = d.Tor.EnsureWithdrawn(torCtx, p.DeploymentID, fence.CutoverID, fence.Onion)
		cancelTor()
		if err != nil {
			return failoverFailure(cmd.ID, p.DeploymentID, "The withdrawn onion could not be verified; the fence was kept.")
		}
	}
	if err := d.removeFailoverFence(p.DeploymentID); err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "The failover fence could not be removed durably.")
	}
	proof.WasFenced = true
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID,
		HostFailoverRelease: proof}
}

func (d *Docker) removeFailoverFence(deploymentID string) error {
	path, err := d.failoverFencePath(deploymentID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if runtime.GOOS != "windows" {
		fd, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer fd.Close()
		if err := fd.Sync(); err != nil {
			return err
		}
	}
	return syncRecoveryDirectory(d.StateDir)
}

// onionTransferRecipient prepares the standby side of an onion transfer. The
// private half stays in the Tor state directory; only the public key returns.
func (d *Docker) onionTransferRecipient(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.OnionTransferRecipientPayload
	if err := cmd.As(&p); err != nil || !failoverDeploymentID.MatchString(p.DeploymentID) ||
		!failoverCutoverID.MatchString(p.CutoverID) || !failoverOnion.MatchString(p.Onion) {
		return failoverFailure(cmd.ID, p.DeploymentID, "Invalid onion transfer recipient request.")
	}
	if d.Tor == nil || !exists(d.appDir(p.DeploymentID)) {
		return failoverFailure(cmd.ID, p.DeploymentID, "The standby application and Tor must be available to receive an onion identity.")
	}
	pub, err := d.Tor.PrepareOnionTransferRecipient(p.DeploymentID, p.CutoverID, p.Onion)
	if err != nil {
		return failoverFailure(cmd.ID, p.DeploymentID, "Onion transfer recipient could not be prepared: "+err.Error())
	}
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID,
		OnionTransferRecipient: &sdkclient.OnionTransferRecipientResult{
			DeploymentID: p.DeploymentID, CutoverID: p.CutoverID, Onion: p.Onion,
			RecipientPubkey: base64.StdEncoding.EncodeToString(pub[:]),
		}}
}

// Fence by exact Docker ID and project label, independent of stale Compose
// files. A crash between tombstone creation and shutdown is retried at startup.
func (d *Docker) stopFencedContainers(ctx context.Context, id string) error {
	if !failoverDeploymentID.MatchString(id) {
		return errors.New("invalid fenced deployment")
	}
	ids, err := d.recoveryContainers(ctx, id)
	if err != nil {
		return err
	}
	if len(ids) > 256 {
		return errors.New("too many fenced containers")
	}
	type observed struct {
		ID         string
		Config     struct{ Labels map[string]string }
		State      struct{ Running, Restarting bool }
		HostConfig struct{ RestartPolicy struct{ Name string } }
	}
	inspect := func(cid string) (observed, error) {
		raw, err := limitedRuntimeOutput(d.dockerCmd(ctx, "inspect", cid), 1024*1024)
		var rows []observed
		if err != nil || json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || rows[0].ID != cid || rows[0].Config.Labels["com.docker.compose.project"] != id {
			return observed{}, errors.New("fenced container ownership changed")
		}
		return rows[0], nil
	}
	for _, cid := range ids {
		if _, err = inspect(cid); err != nil {
			return err
		}
		if _, err = limitedRuntimeOutput(d.dockerCmd(ctx, "update", "--restart=no", cid), 4096); err != nil {
			return err
		}
		if _, err = limitedRuntimeOutput(d.dockerCmd(ctx, "stop", "--time", "30", cid), 4096); err != nil {
			return err
		}
		state, err := inspect(cid)
		if err != nil {
			return err
		}
		if state.State.Running || state.State.Restarting || state.HostConfig.RestartPolicy.Name != "no" {
			return errors.New("fenced container is still restartable")
		}
	}
	final, err := d.recoveryContainers(ctx, id)
	if err != nil {
		return err
	}
	if !slices.Equal(ids, final) {
		return errors.New("fenced containers changed during shutdown")
	}
	return nil
}

// Run under the agent operation lock, before proxy startup or fresh work.
// No fence is cleared here. Removed apps keep their tombstone indefinitely.
func (d *Docker) ReconcileFailoverFences(ctx context.Context) error {
	path := filepath.Join(d.StateDir, "failover-fences")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("unsafe failover fence directory")
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(1025)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 1024 {
		return errors.New("too many failover fences")
	}
	guard, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			return errors.New("unrecognized failover fence file")
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		f, err := d.readFailoverFence(id)
		if err != nil || f == nil {
			return errors.New("failover tombstone cannot be verified")
		}
		if err = d.stopFencedContainers(guard, id); err != nil {
			return err
		}
		if f.Onion != "" && d.Tor != nil {
			if err = d.Tor.EnsureWithdrawn(guard, id, f.CutoverID, f.Onion); err != nil {
				return err
			}
		}
	}
	return nil
}
