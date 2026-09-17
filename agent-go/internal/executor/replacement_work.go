package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const replacementBudget = 40 * time.Minute
const replacementRecordLimit = 4 << 20

var ErrReplacementPending = errors.New("supervised replacement has no confirmed result")

// ReplacementWork identifies one authorized runtime sequence. It permits reading
// its receipt, never launching a second worker or reconstructing a partial job.
type ReplacementWork struct {
	ID            string `json:"id"`
	CommandID     string `json:"command_id"`
	DeploymentID  string `json:"deployment_id"`
	RequestSHA256 string `json:"request_sha256"`
}

func (w *ReplacementWork) Validate() error {
	if w == nil || !workIDPattern.MatchString(w.ID) || w.CommandID == "" || len(w.CommandID) > 200 || !recoveryDeploymentID.MatchString(w.DeploymentID) || !workHashPattern.MatchString(w.RequestSHA256) {
		return errors.New("invalid replacement worker identity")
	}
	return nil
}

type replacementRequest struct {
	Version      int                     `json:"version"`
	ID           string                  `json:"id"`
	CommandID    string                  `json:"command_id"`
	StateDir     string                  `json:"state_dir"`
	Docker       string                  `json:"docker"`
	Env          []string                `json:"env"`
	ConfigSHA256 string                  `json:"config_sha256"`
	Containers   []string                `json:"containers"`
	Payload      sdkclient.DeployPayload `json:"payload"`
	Previous     *runtimeRelease         `json:"previous,omitempty"`
	Redeploy     bool                    `json:"redeploy"`
	Proxy        bool                    `json:"proxy"`
}
type replacementReceipt struct {
	Version       int                    `json:"version"`
	ID            string                 `json:"id"`
	RequestSHA256 string                 `json:"request_sha256"`
	Result        sdkclient.DeployResult `json:"result"`
}
type replacementProgress struct {
	ID            string `json:"id"`
	RequestSHA256 string `json:"request_sha256"`
	Step          string `json:"step"`
}

func (d *Docker) replacementDirectory(id string) (string, error) {
	if !workIDPattern.MatchString(id) || !filepath.IsAbs(d.StateDir) {
		return "", errors.New("invalid replacement worker path")
	}
	dir := filepath.Join(d.StateDir, "operations", "replacement-"+id)
	for _, p := range []string{d.StateDir, filepath.Join(d.StateDir, "operations"), dir} {
		if err := realWorkDirectory(p); err != nil {
			return "", err
		}
	}
	return dir, nil
}
func replacementPayload(p sdkclient.DeployPayload) sdkclient.DeployPayload {
	// The worker needs no source URLs, Git authentication or control-plane token.
	runtime := sdkclient.ManifestRuntime{Type: p.Manifest.Runtime.Type, Startup: p.Manifest.Runtime.Startup, ServiceBindingRetirementProtocol: p.Manifest.Runtime.ServiceBindingRetirementProtocol, ServiceBindingRetirements: p.Manifest.Runtime.ServiceBindingRetirements}
	// A rotation needs its reviewed intent and serving reference so the worker
	// can verify and retire the unused generation. The serving credential is
	// already resolved into Vars; plain bindings are never re-validated there.
	if p.Manifest.Runtime.ServiceBindingRotation != nil {
		runtime.ServiceBindingProtocol = p.Manifest.Runtime.ServiceBindingProtocol
		runtime.ServiceBindings = p.Manifest.Runtime.ServiceBindings
		runtime.ServiceBindingRotationProtocol = p.Manifest.Runtime.ServiceBindingRotationProtocol
		runtime.ServiceBindingRotation = p.Manifest.Runtime.ServiceBindingRotation
	}
	return sdkclient.DeployPayload{DeploymentID: p.DeploymentID, Vars: p.Vars, Routes: p.Routes, ServiceBindingRetirementAuthorizations: p.ServiceBindingRetirementAuthorizations, Manifest: sdkclient.AppManifest{Runtime: runtime, Lifecycle: p.Manifest.Lifecycle}}
}
func (d *Docker) createReplacementWork(cmd *sdkclient.PollCommand, p sdkclient.DeployPayload, previous *runtimeRelease, redeploy bool, containers []string) (*ReplacementWork, error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, err
	}
	docker, err = filepath.Abs(docker)
	if err != nil {
		return nil, err
	}
	app, err := d.recoveryPath(p.DeploymentID)
	if err != nil {
		return nil, err
	}
	hash, err := preparationConfigHash(app)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	w := &ReplacementWork{ID: hex.EncodeToString(nonce[:]), CommandID: cmd.ID, DeploymentID: p.DeploymentID}
	base := filepath.Join(d.StateDir, "operations")
	if err = realWorkDirectory(base); err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "replacement-"+w.ID)
	if err = os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	if err = syncRecoveryDirectory(base); err != nil {
		return nil, err
	}
	request := replacementRequest{Version: 1, ID: w.ID, CommandID: w.CommandID, StateDir: d.StateDir, Docker: docker, Env: preparationEnvironment(d), ConfigSHA256: hash, Containers: containers, Payload: replacementPayload(p), Previous: previous, Redeploy: redeploy, Proxy: d.Proxy != nil}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	w.RequestSHA256 = workHash(raw)
	if err = w.Validate(); err != nil {
		return nil, err
	}
	if err = writePrivateWorkJSON(dir, "request.json", request, replacementRecordLimit); err != nil {
		return nil, err
	}
	return w, nil
}
func (d *Docker) loadReplacementWork(w *ReplacementWork) (string, *replacementRequest, error) {
	if err := w.Validate(); err != nil {
		return "", nil, err
	}
	dir, err := d.replacementDirectory(w.ID)
	if err != nil {
		return "", nil, err
	}
	var r replacementRequest
	raw, err := readPrivateWorkJSON(filepath.Join(dir, "request.json"), &r, replacementRecordLimit)
	if err != nil {
		return "", nil, err
	}
	if workHash(raw) != w.RequestSHA256 || r.Version != 1 || r.ID != w.ID || r.CommandID != w.CommandID || r.Payload.DeploymentID != w.DeploymentID || r.StateDir != d.StateDir || !filepath.IsAbs(r.Docker) || !workHashPattern.MatchString(r.ConfigSHA256) {
		return "", nil, errors.New("replacement request does not match operation")
	}
	return dir, &r, nil
}
func (d *Docker) replacementResult(w *ReplacementWork) (*sdkclient.DeployResult, error) {
	dir, _, err := d.loadReplacementWork(w)
	if err != nil {
		return nil, err
	}
	var r replacementReceipt
	_, err = readPrivateWorkJSON(filepath.Join(dir, "result.json"), &r, replacementRecordLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrReplacementPending
	}
	if err != nil {
		return nil, err
	}
	if r.Version != 1 || r.ID != w.ID || r.RequestSHA256 != w.RequestSHA256 || r.Result.CommandID != w.CommandID || r.Result.DeploymentID != w.DeploymentID || r.Result.ControlToken != "" || !slices.Contains([]string{"success", "failed"}, r.Result.Status) {
		return nil, errors.New("invalid replacement completion receipt")
	}
	return &r.Result, nil
}

// CompletedReplacementWork accepts only the durable final outcome. Process exit,
// present-day container health and a server-side timeout cannot replace a receipt.
func (d *Docker) CompletedReplacementWork(w *ReplacementWork) (*sdkclient.DeployResult, error) {
	r, err := d.replacementResult(w)
	if errors.Is(err, ErrReplacementPending) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, stateErr := exec.CommandContext(ctx, "systemctl", "show", "impreza-replacement-"+w.ID+".service", "--property=ActiveState", "--value").Output()
		if stateErr != nil || !slices.Contains([]string{"active", "activating"}, strings.TrimSpace(string(out))) {
			return nil, errors.New("replacement worker is unavailable without a receipt; review required")
		}
	}
	return r, err
}
func (d *Docker) launchReplacementWork(ctx context.Context, w *ReplacementWork) error {
	if err := w.Validate(); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	escape := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%%"), "$", "$$") }
	args := []string{"--quiet", "--no-ask-password", "--collect", "--unit=impreza-replacement-" + w.ID, "--property=Type=exec", "--property=Restart=no", "--property=UMask=0077", "--property=NoNewPrivileges=yes", "--property=ProtectSystem=full", "--property=ProtectHome=yes", "--property=PrivateTmp=yes", "--property=StandardOutput=null", "--property=StandardError=journal", "--property=RuntimeMaxSec=" + fmt.Sprint(int(replacementBudget.Seconds())+60), "--", escape(binary), "replacement-worker", "--state-dir", escape(d.StateDir), "--work-id", w.ID}
	launchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(launchCtx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start supervised replacement: %w: %s", err, tail(out, 512))
	}
	return nil
}
func validReplacementStep(step string) bool {
	return slices.Contains([]string{"replacing", "checking_health", "installing", "routing", "checking_readiness", "recovering"}, step)
}
func (d *Docker) waitReplacementWork(ctx context.Context, cmd *sdkclient.PollCommand, w *ReplacementWork) sdkclient.DeployResult {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastStep := ""
	for ctx.Err() == nil {
		r, err := d.CompletedReplacementWork(w)
		if err == nil {
			return *r
		}
		if !errors.Is(err, ErrReplacementPending) {
			break
		}
		if dir, dirErr := d.replacementDirectory(w.ID); dirErr == nil {
			var progress replacementProgress
			if _, e := readWorkJSON(filepath.Join(dir, "progress.json"), &progress); e == nil && progress.ID == w.ID && progress.RequestSHA256 == w.RequestSHA256 && validReplacementStep(progress.Step) && progress.Step != lastStep {
				d.deploymentProgress(ctx, cmd, progress.Step)
				lastStep = progress.Step
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: PreparationPendingStatus}
}
func (d *Docker) ForgetReplacementWork(w *ReplacementWork) error {
	if w == nil {
		return nil
	}
	if err := w.Validate(); err != nil {
		return err
	}
	dir, err := d.replacementDirectory(w.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !slices.Contains([]string{"request.json", "started", "progress.json", "result.json"}, entry.Name()) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return errors.New("unexpected replacement worker file; cleanup refused")
		}
	}
	for _, entry := range entries {
		if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	if err = os.Remove(dir); err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(dir))
}

// RunReplacementWorker is a private subprocess entry point. It does not poll or
// contact the API. A saved request grants one execution, including normal startup
// recovery; a crash cannot be retried by invoking the worker again.
func RunReplacementWorker(stateDir, id string) error {
	d := &Docker{StateDir: stateDir, Log: slog.Default()}
	dir, err := d.replacementDirectory(id)
	if err != nil {
		return err
	}
	var request replacementRequest
	raw, err := readPrivateWorkJSON(filepath.Join(dir, "request.json"), &request, replacementRecordLimit)
	if err != nil {
		return err
	}
	w := &ReplacementWork{ID: id, CommandID: request.CommandID, DeploymentID: request.Payload.DeploymentID, RequestSHA256: workHash(raw)}
	if _, _, err = d.loadReplacementWork(w); err != nil {
		return err
	}
	// Apply the same restricted environment to Docker, lifecycle scripts and proxy.
	// Restoration also keeps direct invocation in isolated tests well behaved.
	original := os.Environ()
	os.Clearenv()
	defer func() {
		os.Clearenv()
		for _, v := range original {
			k, value, _ := strings.Cut(v, "=")
			_ = os.Setenv(k, value)
		}
	}()
	for _, v := range request.Env {
		k, value, ok := strings.Cut(v, "=")
		if !ok {
			return errors.New("invalid worker environment")
		}
		if err = os.Setenv(k, value); err != nil {
			return err
		}
	}
	binary, err := exec.LookPath("docker")
	if err != nil {
		return err
	}
	binary, err = filepath.Abs(binary)
	if err != nil || binary != request.Docker {
		return errors.New("replacement Docker executable changed")
	}
	app, err := d.recoveryPath(w.DeploymentID)
	if err != nil {
		return err
	}
	hash, err := preparationConfigHash(app)
	if err != nil {
		return err
	}
	if hash != request.ConfigSHA256 {
		return errors.New("replacement configuration changed before worker start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), replacementBudget)
	defer cancel()
	containers, err := d.recoveryContainers(ctx, w.DeploymentID)
	if err != nil {
		return err
	}
	if !slices.Equal(containers, request.Containers) {
		return errors.New("containers changed before replacement worker start")
	}
	started, err := os.OpenFile(filepath.Join(dir, "started"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = started.Sync()
	closeErr := started.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = syncRecoveryDirectory(dir); err != nil {
		return err
	}
	if request.Proxy {
		d.Proxy = proxy.New(stateDir, d.Log)
	}
	d.Progress = func(_ context.Context, _ *sdkclient.PollCommand, step string) {
		if validReplacementStep(step) {
			if e := writeWorkJSON(dir, "progress.json", replacementProgress{ID: id, RequestSHA256: w.RequestSHA256, Step: step}); e != nil {
				d.Log.Warn("worker progress not persisted", "err", e)
			}
		}
	}
	cmd := &sdkclient.PollCommand{ID: w.CommandID, Kind: sdkclient.CommandDeploy, ProgressProtocol: sdkclient.DeploymentProgressProtocol}
	result := d.finishReplacement(ctx, cmd, request.Payload, request.Previous, request.Redeploy, "")
	if ctx.Err() != nil {
		return errors.New("replacement exceeded worker deadline; review required")
	}
	if result.CommandID != w.CommandID || result.DeploymentID != w.DeploymentID || !slices.Contains([]string{"success", "failed"}, result.Status) {
		return errors.New("replacement did not produce a final result")
	}
	return writePrivateWorkJSON(dir, "result.json", replacementReceipt{Version: 1, ID: id, RequestSHA256: w.RequestSHA256, Result: result}, replacementRecordLimit)
}
