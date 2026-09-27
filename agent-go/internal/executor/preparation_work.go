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
	"sync"
	"time"
)

// PreparationPendingStatus stays inside the executor/poller boundary. It must
// never be sent as a deployment result or release the operation journal.
const PreparationPendingStatus = "preparation_pending"

var ErrPreparationPending = errors.New("supervised preparation has no confirmed result")
var workIDPattern = regexp.MustCompile("^[a-f0-9]{32}$")
var workHashPattern = regexp.MustCompile("^[a-f0-9]{64}$")

// preparationUnitStopTimeout bounds the confirmation that a wedged worker
// unit went inactive after `systemctl stop`. A var so tests can shrink it.
var preparationUnitStopTimeout = 30 * time.Second

// preparationUnitStopGrace is the worker unit's TimeoutStopSec: how long
// systemd waits after SIGTERM before it kills the unit. It stays below
// preparationUnitStopTimeout — with systemd's 90 s default, a worker that
// ignores SIGTERM always outlived the confirmation window.
const preparationUnitStopGrace = 20 * time.Second

// preparationLivenessInterval is how often the supervised wait asks systemd
// whether the worker unit is still alive. A var so tests can shrink it.
var preparationLivenessInterval = 2 * time.Second

// buildNoReceiptCeiling bounds the wait for a build worker's receipt. It
// sits past the unit's RuntimeMaxSec (the build budget + 60 s) with slack:
// by then systemd has killed the worker, and a receipt that did not land
// will not land later. A var so tests can shrink it.
var buildNoReceiptCeiling = composeBuildTimeout + 60*time.Second + 30*time.Second

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
	OwnedBuilder      bool     `json:"owned_builder,omitempty"`
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
	RecoveryBootID string `json:"recovery_boot_id,omitempty"`
	Interrupted    bool   `json:"interrupted,omitempty"`
	Version        int    `json:"version"`
	ID             string `json:"id"`
	RequestSHA256  string `json:"request_sha256"`
	Completed      bool   `json:"completed"`
	Success        bool   `json:"success"`
	Output         string `json:"output,omitempty"`
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
		// The systemd unit's RuntimeMaxSec must sit above the worker's
		// own progress-based deadline so the internal budget always fires
		// first and writes its receipt.
		return composePullCeiling
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
	owned := false
	if step == "build" {
		var policyErr error
		owned, policyErr = d.ControlledBuildsEnabled()
		if policyErr != nil {
			return nil, policyErr
		}
		if owned {
			if cmd.ControlToken == "" || cmd.ProgressProtocol != sdkclient.DeploymentProgressProtocol {
				return nil, errors.New("controlled builds require authenticated deployment control and durable progress")
			}
			if err := d.CheckControlledBuilds(context.Background()); err != nil {
				return nil, err
			}
		}
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
	request := preparationWorkRequest{OwnedBuilder: owned, BootID: currentBootID(), PrivateBuild: privateBuild, Version: 1, ID: work.ID, CommandID: cmd.ID, DeploymentID: id, StateDir: d.StateDir, Step: step, Docker: docker, Env: preparationEnvironment(d), ConfigSHA256: hash}
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
	if request.OwnedBuilder && (w.Step != "build" || !request.LocalBootRecovery || request.BootID == "") {
		return "", nil, errors.New("owned builder requires a bound local build request")
	}
	return dir, &request, nil
}
func (d *Docker) preparationWorkResult(w *PreparationWork, id string) (*preparationWorkResult, error) {
	dir, request, err := d.loadPreparationWork(w, id)
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
	if result.RecoveryBootID != "" {
		if !request.OwnedBuilder || !bootIDPattern.MatchString(result.RecoveryBootID) || !result.Completed || result.Success || result.Interrupted {
			return nil, errors.New("invalid recovered preparation receipt")
		}
		if err := d.confirmOwnedRecovery(w, id, result.RecoveryBootID); err != nil {
			return nil, err
		}
	} else if result.Interrupted {
		if !request.OwnedBuilder || !result.Completed || result.Success {
			return nil, errors.New("invalid interrupted preparation receipt")
		}
		if err := d.confirmOwnedPreparationStopped(w, id); err != nil {
			return nil, err
		}
	} else if request.OwnedBuilder && result.Completed {
		if err := d.confirmOwnedPreparationPhase(w, id, "finished"); err != nil {
			return nil, err
		}
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
		// A live service explains why a receipt is pending, and a unit state
		// systemd could not report proves nothing: both stay pending and are
		// checked again. A stopping unit is still alive. Only a unit
		// reported stopped counts as a worker gone without a receipt — after
		// one more read, since the worker writes its receipt just before it
		// exits. That absence is never taken for success, and nothing is
		// replayed.
		if alive, known := preparationUnitState(context.Background(), r.Work); alive || !known {
			return nil, ErrPreparationPending
		}
		result, err = d.preparationWorkResult(r.Work, r.DeploymentID)
		if errors.Is(err, ErrPreparationPending) {
			// A worker with no receipt and no live unit cannot still be
			// changing anything the deployment depends on: a pull touches
			// only the image cache, a plain build only produces an image
			// nothing uses yet. Reconciliation stays a defined, retryable
			// failure instead of a "review required" that held the whole
			// command queue forever. Only the controlled builder
			// keeps review: its own recovery owns that state.
			if r.Work.Step == "pull" || !d.preparationOwnedBuilder(r) {
				next := *r
				next.Phase = "aborted"
				return &next, next.Validate()
			}
			return nil, errors.New("controlled build worker is unavailable without a receipt; review required")
		}
	}
	if err != nil {
		return nil, err
	}
	if result.Interrupted || result.RecoveryBootID != "" {
		next := *r
		next.Phase = "aborted"
		return &next, next.Validate()
	}
	if !result.Completed || !result.Success {
		// A worker that wrote its receipt has finished — without a verdict
		// when its own budget killed the build (every build over ten
		// minutes ends that way). A pull only touched the image cache and a
		// plain build only an unused image, and the build credentials are
		// cleared when the configuration is restored, so the receipt
		// reconciles into the defined, retryable "aborted" path.
		// The controlled builder keeps review: its recovery owns the
		// builder state.
		if r.Work.Step == "pull" || !d.preparationOwnedBuilder(r) {
			next := *r
			next.Phase = "aborted"
			return &next, next.Validate()
		}
		return nil, errors.New("controlled build worker did not record successful completion; review required")
	}
	next := *r
	next.Phase = "ready"
	return &next, nil
}

// preparationSupervised decides whether a preparation step goes to a worker.
// Pulls always do. A Blocked deployment (onion, data_dir) keeps its build
// synchronous under the step deadline, as before the supervisor covered
// Blocked deployments — except on a host that builds under the
// controlled builder, which only the worker runs: building in the root
// daemon there would bypass the admin's limits while the host advertises
// controlled builds. A policy that cannot be read goes to the
// worker too, which refuses it with the right error.
func (d *Docker) preparationSupervised(step string, recovery *PreparationRecovery) bool {
	switch {
	case step == "pull":
		return true
	case step != "build" || recovery == nil:
		return false
	case !recovery.Blocked:
		return true
	}
	controlled, policyErr := d.ControlledBuildsEnabled()
	return controlled || policyErr != nil
}

// preparationOwnedBuilder reports whether the work ran under the controlled
// builder. A request that cannot be read or verified counts as owned: review
// is the conservative answer for state this agent cannot identify.
func (d *Docker) preparationOwnedBuilder(r *PreparationRecovery) bool {
	_, request, err := d.loadPreparationWork(r.Work, r.DeploymentID)
	return err != nil || request.OwnedBuilder
}

// PreparationWorkerGone reports whether nothing of the work can still be
// changing images: its unit is confirmed stopped (an unreadable state is not
// a stop), and it is not a controlled build — the builder container can
// outlive the unit, and its own recovery owns that state. A request that
// cannot be read counts as a controlled build.
func (d *Docker) PreparationWorkerGone(r *PreparationRecovery) bool {
	if r == nil || r.Work == nil || d.preparationOwnedBuilder(r) {
		return false
	}
	alive, known := preparationUnitState(context.Background(), r.Work)
	return known && !alive
}

func (d *Docker) launchPreparationWork(ctx context.Context, w *PreparationWork) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	escape := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%%"), "$", "$$") }
	args := []string{"--quiet", "--no-ask-password", "--collect", "--unit=impreza-preparation-" + w.ID, "--property=Type=exec", "--property=Restart=no", "--property=UMask=0077", "--property=NoNewPrivileges=yes", "--property=ProtectSystem=full", "--property=ProtectHome=yes", "--property=PrivateTmp=yes", "--property=StandardOutput=null", "--property=StandardError=journal", "--property=RuntimeMaxSec=" + fmt.Sprint(int(workBudget(w.Step).Seconds())+60), "--property=TimeoutStopSec=" + fmt.Sprint(int(preparationUnitStopGrace.Seconds())), "--", escape(binary), "preparation-worker", "--state-dir", escape(d.StateDir), "--work-id", w.ID}
	launchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(launchCtx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start supervised preparation: %w: %s", err, tail(out, 512))
	}
	return nil
}

// waitPreparationWork waits for the worker's receipt on the deploy's
// context, which has no deadline — so the wait bounds itself: past
// the no-receipt ceiling, and as soon as systemd reports the unit gone
// without a receipt. A pull then ends in the defined, retryable failure;
// a build goes back to the recovery path as pending, which fails a plain
// build the same defined way and keeps the controlled builder in review.
func (d *Docker) waitPreparationWork(ctx context.Context, w *PreparationWork, id string, commands ...*sdkclient.PollCommand) ([]byte, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	nextControl := time.Now()
	started := time.Now()
	nextLiveness := started.Add(preparationLivenessInterval)
	goneOnce := false
	// The worker's own budget fires at its ceiling and writes a receipt
	// within seconds, and the unit's RuntimeMaxSec (budget + 60 s) is the
	// last resort. The pull branch fires at its ceiling + 2 ticks and stops
	// the unit itself; the build ceiling sits past RuntimeMaxSec.
	ceiling := buildNoReceiptCeiling
	if w.Step == "pull" {
		ceiling = composePullCeiling + 2*pullTick
	}
	for {
		if ctx.Err() != nil {
			return nil, ErrPreparationPending
		}
		result, err := d.preparationWorkResult(w, id)
		if err == nil {
			if result.Interrupted {
				return nil, errDeployCancelled
			}
			if !result.Completed {
				// A pull receipt without a verdict stays defined: a pull
				// only populates the local image cache. Waiting
				// forever for a dead worker to change its mind is what
				// froze entire hosts behind "review required".
				if w.Step == "pull" {
					return []byte(result.Output), errors.New("supervised pull ended without a confirmed result; the pull only populates the local image cache, so the deployment can be retried")
				}
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
		if time.Since(started) > ceiling {
			if w.Step != "pull" {
				// Past RuntimeMaxSec the build ran out of time or died without
				// a receipt. The recovery path decides: a plain build fails
				// the defined way, the controlled builder keeps its review.
				return nil, fmt.Errorf("%w: supervised build worker produced no receipt past its unit deadline", ErrPreparationPending)
			}
			// The unit may still be wedged (an older worker blocked in
			// Run on inherited pipes, a hung Docker call). Stop it and
			// confirm it went inactive before declaring the failure:
			// reporting while a pull worker might still run would race
			// a late receipt. An unconfirmed stop goes back
			// to the recovery path, which checks again, instead of failing
			// the deploy with the unit maybe alive.
			if err := stopPreparationUnit(w); err != nil {
				return nil, fmt.Errorf("%w: supervised pull worker produced no receipt and could not be confirmed stopped (%v); review required", ErrPreparationPending, err)
			}
			return nil, errors.New("supervised pull worker produced no receipt within its budget ceiling; the pull only populates the local image cache, so the deployment can be retried")
		}
		// A worker that died without a receipt (OOM, a failed start, a full
		// disk at the receipt write) never writes one, and systemd says so
		// long before any ceiling: a dead pull worker used to hold the queue
		// for 45 minutes, a dead build worker forever. Believe it only
		// when two readings agree and the receipt is still absent after the
		// second; a state systemd could not report proves nothing.
		if time.Now().After(nextLiveness) {
			nextLiveness = time.Now().Add(preparationLivenessInterval)
			alive, known := preparationUnitState(ctx, w)
			switch {
			case alive || !known:
				goneOnce = false
			case !goneOnce:
				goneOnce = true
			default:
				if _, again := d.preparationWorkResult(w, id); !errors.Is(again, ErrPreparationPending) {
					continue
				}
				if w.Step == "pull" {
					// The image-cache-only side effect keeps this a defined,
					// retryable failure.
					return nil, errors.New("supervised pull worker exited without a receipt; the pull only populates the local image cache, so the deployment can be retried")
				}
				return nil, fmt.Errorf("%w: supervised build worker exited without a receipt", ErrPreparationPending)
			}
		}
		if len(commands) == 1 && time.Now().After(nextControl) {
			// Errors never count as cancellation, and the worker is never killed by PID.
			// A durable stopped journal still needs its matching worker receipt.
			_, _ = d.InterruptPreparationWork(ctx, commands[0], w, id)
			nextControl = time.Now().Add(2 * time.Second)
		}
		select {
		case <-ctx.Done():
			return nil, ErrPreparationPending
		case <-ticker.C:
		}
	}
}

// preparationUnitState reads the worker unit's ActiveState. alive holds in
// every state in which the worker may still run or still be stopping —
// deactivating included. known is false when systemd could not answer: an
// unreadable state is never taken for a dead worker. A collected
// unit reads inactive.
func preparationUnitState(ctx context.Context, w *PreparationWork) (alive, known bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "impreza-preparation-"+w.ID+".service", "--property=ActiveState", "--value").Output()
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(out)) {
	case "inactive", "failed":
		return false, true
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return true, true
	}
	return false, false
}

// settleBlockedWork ends the pending outcome of a Blocked deployment's
// worker (a failed launch, an unreadable receipt, the agent shutting down)
// as a failure. A Blocked deployment must never end pending: its
// failure path is what removes a first deploy's onion service and
// restores the configuration, and its recovery never reconciles. The unit
// is stopped first; the defined, retryable failure needs it confirmed
// inactive, and an unconfirmed stop still fails — with a review note. Only
// a controlled build reaches here as a build (a Blocked deployment builds
// synchronously otherwise), and its builder container can outlive the unit:
// that failure always carries the review note.
func settleBlockedWork(w *PreparationWork, cause error) error {
	what := "supervised pull"
	if w.Step == "build" {
		what = "controlled build"
	}
	if err := stopPreparationUnit(w); err != nil {
		return fmt.Errorf("%s did not finish (%v) and its worker could not be confirmed stopped (%v); review the host before retrying", what, cause, err)
	}
	if w.Step == "build" {
		return fmt.Errorf("%s did not finish (%v); its worker is confirmed stopped, but the controlled builder may still hold state; review the host before retrying", what, cause)
	}
	return fmt.Errorf("%s did not finish (%v); its worker is confirmed stopped, and the pull only populates the local image cache, so the deployment can be retried", what, cause)
}

// stopPreparationUnit halts a wedged preparation worker unit and confirms
// it went inactive. Only a confirmed-inactive unit may be reported as a
// defined failure; anything else stays "review required".
func stopPreparationUnit(w *PreparationWork) error {
	unit := "impreza-preparation-" + w.ID + ".service"
	ctx, cancel := context.WithTimeout(context.Background(), preparationUnitStopTimeout+15*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "systemctl", "stop", unit).Run()
	deadline := time.Now().Add(preparationUnitStopTimeout)
	for {
		out, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=ActiveState", "--value").Output()
		if err != nil {
			return fmt.Errorf("unit state unreadable: %w", err)
		}
		switch state := strings.TrimSpace(string(out)); state {
		case "inactive", "failed":
			return nil
		case "active", "activating", "deactivating":
			if time.Now().After(deadline) {
				return fmt.Errorf("unit still %s after stop", state)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		default:
			return fmt.Errorf("unit in unexpected state %q", state)
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
	var request preparationWorkRequest
	if _, err = readWorkJSON(filepath.Join(dir, "request.json"), &request); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A missing request was removed by an earlier refused cleanup, which
	// only a plain worker's cleanup does (a controlled build goes to its own
	// cleanup first): what remains follows the plain rules below.
	if request.OwnedBuilder {
		return d.forgetOwnedPreparation(w, request.DeploymentID, dir)
	}
	// Known files, plus the temporary file writeAtomic leaves when a worker
	// is killed mid-write. Anything else keeps the directory for review —
	// but never the files that can carry credentials (request.json holds
	// the proxy environment, and a temporary file can hold part of it) or
	// build output.
	unexpected := false
	for _, entry := range entries {
		name := entry.Name()
		known := slices.Contains([]string{"request.json", "started", "result.json"}, name) || strings.HasPrefix(name, ".impreza-tmp-")
		if !known || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			unexpected = true
		}
	}
	if unexpected {
		for _, entry := range entries {
			name := entry.Name()
			if name != "request.json" && name != "result.json" && !strings.HasPrefix(name, ".impreza-tmp-") {
				continue
			}
			if info, statErr := os.Lstat(filepath.Join(dir, name)); statErr == nil && info.Mode().IsRegular() {
				if err = os.Remove(filepath.Join(dir, name)); err != nil {
					return err
				}
			}
		}
		_ = syncRecoveryDirectory(dir)
		return errors.New("unexpected preparation worker file; credentials removed, directory kept for review")
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
	// A pull runs on the progress budget: the deadline extends while
	// registry bytes or command output arrive and hard-caps at
	// composePullCeiling. The systemd unit's RuntimeMaxSec uses the
	// same ceiling, so the internal deadline always fires first and the
	// receipt carries the reason. Build keeps its fixed budget.
	output := &preparationTail{limit: 4096}
	var budget *pullBudget
	var ctx context.Context
	var cancel context.CancelFunc
	if w.Step == "pull" {
		ctx, cancel = context.WithCancel(context.Background())
		budget = newPullBudget(cancel, output, hostRxBytes)
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), workBudget(w.Step))
	}
	defer cancel()
	if request.OwnedBuilder {
		return d.runOwnedPreparation(ctx, w, &request, dir, app)
	}
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
	command.Stdout = output
	command.Stderr = output
	// A docker CLI plugin (compose) can outlive the CLI and keep holding
	// the output pipes; without a bounded wait and a process-group kill,
	// Run() would never return after the budget fired and no receipt would
	// land.
	prepareWorkerCommand(command)
	if request.PrivateBuild {
		command.Stdout = io.Discard
		command.Stderr = io.Discard
	}
	err = command.Run()
	budgetReason := ""
	if budget != nil {
		budgetReason = budget.stop()
	}
	if request.PrivateBuild {
		output.data = []byte("Build output withheld because private credentials were mounted.")
		if cleanupErr := clearBuildSecrets(app); cleanupErr != nil {
			err = errors.New("Build credential cleanup failed")
		}
	}
	// The budget's own expiry is a defined failure: the pull only fills
	// the local image cache, so a retry is always safe. Any other
	// signal death keeps the incomplete-receipt semantics.
	completed := true
	if err != nil && budgetReason != "" {
		output.appendNote(budgetReason)
	} else if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() < 0 {
			completed = false
		}
	}
	result := preparationWorkResult{Version: 1, ID: id, RequestSHA256: w.RequestSHA256, Completed: completed, Success: err == nil, Output: string(output.data)}
	return writeWorkJSON(dir, "result.json", result)
}

type preparationTail struct {
	mu    sync.Mutex
	data  []byte
	total int64
	limit int
}

func (b *preparationTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return n, nil
}

// Total reports every byte the command ever wrote. It grows past the
// retained window on purpose: it is the pull watchdog's output-progress
// signal, and a capped length would stop growing mid-extraction and read
// as a stall.
func (b *preparationTail) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// appendNote records a budget reason at the tail end, after the command
// finished, so the retained 4 KB window leads with the diagnosis.
func (b *preparationTail) appendNote(note string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, '\n')
	b.data = append(b.data, note...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
}
