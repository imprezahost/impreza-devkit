package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// PreparationPendingStatus stays inside the executor/poller boundary. It must
// never be sent as a deployment result or release the operation journal.
const PreparationPendingStatus = "preparation_pending"

var ErrPreparationPending = errors.New("supervised preparation has no confirmed result")
var workIDPattern = regexp.MustCompile("^[a-f0-9]{32}$")
var workHashPattern = regexp.MustCompile("^[a-f0-9]{64}$")

// PreparationWork binds one detached worker to the exact durable operation.
// It authorizes checking a receipt, never relaunching the worker.
type PreparationWork struct {
	ID            string `json:"id"`
	Step          string `json:"step"`
	CommandID     string `json:"command_id"`
	RequestSHA256 string `json:"request_sha256"`
}

func (w *PreparationWork) validate() error {
	if w == nil || !workIDPattern.MatchString(w.ID) || !slices.Contains([]string{"pull", "build"}, w.Step) || w.CommandID == "" || len(w.CommandID) > 200 || !workHashPattern.MatchString(w.RequestSHA256) {
		return errors.New("invalid preparation worker identity")
	}
	return nil
}

type preparationWorkRequest struct {
	LocalBootRecovery bool     `json:"local_boot_recovery,omitempty"`
	BootID            string   `json:"boot_id,omitempty"`
	PrivateBuild      bool     `json:"private_build,omitempty"`
	Version           int      `json:"version"`
	ID                string   `json:"id"`
	CommandID         string   `json:"command_id"`
	DeploymentID      string   `json:"deployment_id"`
	StateDir          string   `json:"state_dir"`
	Step              string   `json:"step"`
	Docker            string   `json:"docker"`
	Env               []string `json:"env"`
	ConfigSHA256      string   `json:"config_sha256"`
}
type preparationWorkResult struct {
	Version       int    `json:"version"`
	ID            string `json:"id"`
	RequestSHA256 string `json:"request_sha256"`
	Completed     bool   `json:"completed"`
	Success       bool   `json:"success"`
	Output        string `json:"output,omitempty"`
}

func workHash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

// Probe before accepting work. Unsupported development environments retain the
// original synchronous executor; never fall back after a launch was attempted.
func supportsPreparationWorkers() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	info, err := os.Stat("/run/systemd/system")
	if err != nil || !info.IsDir() {
		return false
	}
	for _, binary := range []string{"systemd-run", "systemctl"} {
		if _, err := exec.LookPath(binary); err != nil {
			return false
		}
	}
	return true
}

func workBudget(step string) time.Duration {
	if step == "pull" {
		return composePullTimeout
	}
	return composeBuildTimeout
}

// Only the Docker execution environment is inherited by the detached service.
// API credentials and unrelated process variables are not needed by this worker.
func preparationEnvironment(d *Docker) []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key == "DOCKER_CONFIG" {
			continue
		}
		if slices.Contains([]string{"PATH", "HOME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy"}, key) || strings.HasPrefix(key, "DOCKER_") || strings.HasPrefix(key, "COMPOSE_") || strings.HasPrefix(key, "BUILDKIT_") || strings.HasPrefix(key, "BUILDX_") {
			env = append(env, value)
		}
	}
	return append(env, "DOCKER_CONFIG="+filepath.Join(d.StateDir, ".docker"))
}
func realWorkDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("preparation worker path is not a real directory")
	}
	return nil
}
func (d *Docker) workDirectory(id string) (string, error) {
	if !workIDPattern.MatchString(id) || !filepath.IsAbs(d.StateDir) {
		return "", errors.New("invalid preparation worker path")
	}
	dir := filepath.Join(d.StateDir, "operations", "preparation-"+id)
	for _, p := range []string{d.StateDir, filepath.Join(d.StateDir, "operations"), dir} {
		if err := realWorkDirectory(p); err != nil {
			return "", err
		}
	}
	return dir, nil
}
func readWorkJSON(path string, value any) ([]byte, error) {
	return readPrivateWorkJSON(path, value, 65536)
}
func readPrivateWorkJSON(path string, value any, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invalid private preparation worker file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(value); err != nil {
		return nil, err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing preparation worker data")
	}
	return raw, nil
}
func writeWorkJSON(dir, name string, value any) error {
	return writePrivateWorkJSON(dir, name, value, 65536)
}
func writePrivateWorkJSON(dir, name string, value any, limit int) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return errors.New("preparation worker record exceeds size limit")
	}
	if err = writeAtomic(filepath.Join(dir, name), raw, 0600); err != nil {
		return err
	}
	return syncRecoveryDirectory(dir)
}
func preparationConfigHash(dir string) (string, error) {
	snapshot, err := captureDeployConfig(dir, true)
	if err != nil {
		return "", err
	}
	var files []PreparationFile
	for _, f := range snapshot {
		files = append(files, PreparationFile{Name: f.name, Data: f.data, Mode: uint32(f.mode), Exists: f.exists})
	}
	raw, err := json.Marshal(files)
	return workHash(raw), err
}
func (d *Docker) createPreparationWork(cmd *sdkclient.PollCommand, id, step string) (*PreparationWork, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("supervised preparation requires Linux and systemd")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return nil, err
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, err
	}
	docker, err = filepath.Abs(docker)
	if err != nil {
		return nil, err
	}
	dir, err := d.recoveryPath(id)
	if err != nil {
		return nil, err
	}
	hash, err := preparationConfigHash(dir)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	work := &PreparationWork{ID: hex.EncodeToString(nonce[:]), CommandID: cmd.ID, Step: step}
	base := filepath.Join(d.StateDir, "operations")
	if err = realWorkDirectory(base); err != nil {
		return nil, err
	}
	path := filepath.Join(base, "preparation-"+work.ID)
	if err = os.Mkdir(path, 0700); err != nil {
		return nil, err
	}
	if err = syncRecoveryDirectory(base); err != nil {
		return nil, err
	}
	var payload sdkclient.DeployPayload
	if len(cmd.Payload) > 0 {
		if err := cmd.As(&payload); err != nil {
			return nil, err
		}
	}
	privateBuild := step == "build" && payload.Manifest.Runtime.Build != nil && len(payload.Manifest.Runtime.Build.SecretNames) > 0
	request := preparationWorkRequest{BootID: currentBootID(), PrivateBuild: privateBuild, Version: 1, ID: work.ID, CommandID: cmd.ID, DeploymentID: id, StateDir: d.StateDir, Step: step, Docker: docker, Env: preparationEnvironment(d), ConfigSHA256: hash}
	request.LocalBootRecovery = request.BootID != "" && localPreparationDaemon(&request) == nil
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	work.RequestSHA256 = workHash(raw)
	if err = work.validate(); err != nil {
		return nil, err
	}
	if err = writeWorkJSON(path, "request.json", request); err != nil {
		return nil, err
	}
	return work, nil
}
func (d *Docker) loadPreparationWork(w *PreparationWork, id string) (string, *preparationWorkRequest, error) {
	if err := w.validate(); err != nil {
		return "", nil, err
	}
	dir, err := d.workDirectory(w.ID)
	if err != nil {
		return "", nil, err
	}
	var request preparationWorkRequest
	raw, err := readWorkJSON(filepath.Join(dir, "request.json"), &request)
	if err != nil {
		return "", nil, err
	}
	if (request.BootID != "" && !bootIDPattern.MatchString(request.BootID)) || workHash(raw) != w.RequestSHA256 || request.Version != 1 || request.ID != w.ID || request.CommandID != w.CommandID || request.DeploymentID != id || request.StateDir != d.StateDir || request.Step != w.Step {
		return "", nil, errors.New("preparation worker request does not match operation")
	}
	return dir, &request, nil
}
func (d *Docker) preparationWorkResult(w *PreparationWork, id string) (*preparationWorkResult, error) {
	dir, _, err := d.loadPreparationWork(w, id)
	if err != nil {
		return nil, err
	}
	var result preparationWorkResult
	_, err = readWorkJSON(filepath.Join(dir, "result.json"), &result)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrPreparationPending
	}
	if err != nil {
		return nil, err
	}
	if result.Version != 1 || result.ID != w.ID || result.RequestSHA256 != w.RequestSHA256 || (result.Success && !result.Completed) {
		return nil, errors.New("preparation receipt belongs to another worker or is invalid")
	}
	return &result, nil
}

// CompletedPreparationWork promotes only a successful, durable worker receipt.
// A verified host reboot may instead produce an aborted checkpoint. Neither
// missing receipts nor a reboot are ever reported as successful deployment.
func (d *Docker) CompletedPreparationWork(r *PreparationRecovery, commandID string) (*PreparationRecovery, error) {
	if r == nil || r.Validate() != nil || r.Phase != "busy" || r.Work == nil || r.Work.CommandID != commandID {
		return nil, errors.New("no supervised preparation to reconcile")
	}
	result, err := d.preparationWorkResult(r.Work, r.DeploymentID)
	if errors.Is(err, ErrPreparationPending) {
		if interrupted, rebootErr := d.preparationAfterReboot(r); rebootErr == nil {
			return interrupted, nil
		}
		// A live service explains why a receipt is pending. Its absence is only
		// a reason to request review, never permission to restore or replay.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, stateErr := exec.CommandContext(ctx, "systemctl", "show", "impreza-preparation-"+r.Work.ID+".service", "--property=ActiveState", "--value").Output()
		if stateErr != nil || !slices.Contains([]string{"active", "activating"}, strings.TrimSpace(string(out))) {
			return nil, errors.New("preparation worker is unavailable without a receipt; review required")
		}
	}
	if err != nil {
		return nil, err
	}
	if !result.Completed || !result.Success {
		return nil, errors.New("preparation worker did not record successful completion; review required")
	}
	next := *r
	next.Phase = "ready"
	return &next, nil
}
func (d *Docker) launchPreparationWork(ctx context.Context, w *PreparationWork) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	escape := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%%"), "$", "$$") }
	args := []string{"--quiet", "--no-ask-password", "--collect", "--unit=impreza-preparation-" + w.ID, "--property=Type=exec", "--property=Restart=no", "--property=UMask=0077", "--property=NoNewPrivileges=yes", "--property=ProtectSystem=full", "--property=ProtectHome=yes", "--property=PrivateTmp=yes", "--property=StandardOutput=null", "--property=StandardError=journal", "--property=RuntimeMaxSec=" + fmt.Sprint(int(workBudget(w.Step).Seconds())+60), "--", escape(binary), "preparation-worker", "--state-dir", escape(d.StateDir), "--work-id", w.ID}
	launchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(launchCtx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start supervised preparation: %w: %s", err, tail(out, 512))
	}
	return nil
}
func (d *Docker) waitPreparationWork(ctx context.Context, w *PreparationWork, id string) ([]byte, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil, ErrPreparationPending
		}
		result, err := d.preparationWorkResult(w, id)
		if err == nil {
			if !result.Completed {
				return nil, ErrPreparationPending
			}
			if !result.Success {
				return []byte(result.Output), errors.New("supervised Docker preparation failed")
			}
			return []byte(result.Output), nil
		}
		if !errors.Is(err, ErrPreparationPending) {
			return nil, fmt.Errorf("%w: %v", ErrPreparationPending, err)
		}
		select {
		case <-ctx.Done():
			return nil, ErrPreparationPending
		case <-ticker.C:
		}
	}
}

// ForgetPreparationWork removes only known files after a safe checkpoint or ACK.
func (d *Docker) ForgetPreparationWork(w *PreparationWork) error {
	if w == nil {
		return nil
	}
	if err := w.validate(); err != nil {
		return err
	}
	dir, err := d.workDirectory(w.ID)
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
		if !slices.Contains([]string{"request.json", "started", "result.json"}, entry.Name()) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return errors.New("unexpected preparation worker file; cleanup refused")
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

// RunPreparationWorker is an internal subprocess entry point. It can execute
// exactly one pull/build, never replace containers or contact the control plane.
func RunPreparationWorker(stateDir, id string) error {
	d := &Docker{StateDir: stateDir}
	dir, err := d.workDirectory(id)
	if err != nil {
		return err
	}
	var request preparationWorkRequest
	raw, err := readWorkJSON(filepath.Join(dir, "request.json"), &request)
	if err != nil {
		return err
	}
	w := &PreparationWork{ID: id, CommandID: request.CommandID, Step: request.Step, RequestSHA256: workHash(raw)}
	if _, _, err = d.loadPreparationWork(w, request.DeploymentID); err != nil {
		return err
	}
	if request.BootID != "" && request.BootID != currentBootID() {
		return errors.New("preparation worker belongs to an earlier host boot; never replay")
	}
	if request.LocalBootRecovery {
		if err := localPreparationDaemon(&request); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(request.Docker) || !recoveryDeploymentID.MatchString(request.DeploymentID) {
		return errors.New("invalid preparation worker command")
	}
	app, err := d.recoveryPath(request.DeploymentID)
	if err != nil {
		return err
	}
	configHash, err := preparationConfigHash(app)
	if err != nil {
		return err
	}
	if configHash != request.ConfigSHA256 {
		return errors.New("preparation inputs changed before worker start")
	}
	// O_EXCL plus directory sync prevents duplicate launches, including after a
	// worker crash. An existing started marker is never treated as a retry request.
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
	ctx, cancel := context.WithTimeout(context.Background(), workBudget(w.Step))
	defer cancel()
	args := []string{"compose", w.Step}
	if w.Step == "pull" {
		args = append(args, "--ignore-buildable")
	}
	if request.PrivateBuild {
		args = append(args, "--no-cache")
	}
	command := exec.CommandContext(ctx, request.Docker, args...)
	command.Dir = app
	command.Env = request.Env
	output := &preparationTail{limit: 4096}
	command.Stdout = output
	command.Stderr = output
	if request.PrivateBuild {
		command.Stdout = io.Discard
		command.Stderr = io.Discard
	}
	err = command.Run()
	if request.PrivateBuild {
		output.data = []byte("Build output withheld because private credentials were mounted.")
		if cleanupErr := clearBuildSecrets(app); cleanupErr != nil {
			err = errors.New("Build credential cleanup failed")
		}
	}
	result := preparationWorkResult{Version: 1, ID: id, RequestSHA256: w.RequestSHA256, Completed: ctx.Err() == nil, Success: err == nil && ctx.Err() == nil, Output: string(output.data)}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() < 0 {
			result.Completed = false
		}
	}
	return writeWorkJSON(dir, "result.json", result)
}

type preparationTail struct {
	data  []byte
	limit int
}

func (b *preparationTail) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return n, nil
}
