package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Ready-swap redeploy (ready-swap-v1).
//
// A redeploy of a single-service web app behind the proxy normally lets
// Compose stop the running container and start the new one, and the route
// answers 502 in between. With ReadySwap the agent instead:
//
//  1. starting_next: starts the new version as `<id>-app-next` next to the
//     running `<id>-app`, and proves it ready: the container running (and
//     healthy, if it has a healthcheck) and an HTTP request to it over the
//     proxy network, with the app's Host, answering the accepted status three
//     times in a row within the timeout;
//  2. routed_next: moves the deployment's upstream to `-app-next` (graceful
//     Caddy reload) and lets the old container drain;
//  3. replacing_app: recreates `<id>-app` with the new version (no traffic
//     reaches it) and proves it ready the same way;
//  4. routed_app: moves the upstream back to `-app`, drains, removes
//     `-app-next`.
//
// Every other part of the agent (metrics, logs, backups, egress, restart,
// uninstall) keeps seeing the one `<id>-app` container it always had. The
// new version starts twice; that is the price of ending in the canonical
// shape.
//
// The journal (<app>/ready-swap.json, 0600) records the phase before each
// step. An interrupted swap is settled on exactly one serving version:
// before the new `app` exists, the previous version keeps (or gets back)
// the route and `-app-next` is removed; after, the new `app` is proven and
// takes the route. Never both stopped, never both kept.
//
// Only apps that qualify are swapped: one service named `app`, attached to
// the proxy network, no host port, no writable volume, no Tor egress or
// sandbox rewrite, no database binding lifecycle in flight. A payload that
// asks for a swap on anything else is refused before any container changes.

const (
	readySwapJournalName = "ready-swap.json"
	readySwapComposeName = "ready-swap.compose.json"
	readySwapPasses      = 3
)

const (
	swapStartingNext = "starting_next"
	swapRoutedNext   = "routed_next"
	swapReplacingApp = "replacing_app"
	swapRoutedApp    = "routed_app"
	swapDone         = "done"
)

var readySwapPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@/%-]{0,255}$`)

type readySwapRecord struct {
	Version      int                       `json:"version"`
	DeploymentID string                    `json:"deployment_id"`
	CommandID    string                    `json:"command_id"`
	WorkID       string                    `json:"work_id,omitempty"`
	Phase        string                    `json:"phase"`
	ReleaseID    string                    `json:"release_id"`
	Port         string                    `json:"port"`
	Host         string                    `json:"host"`
	Policy       sdkclient.ReadySwapPolicy `json:"policy"`
	// Pair: the routing fragment carries both upstreams, so the
	// flip is container lifecycle (stop the serving one) and recovery
	// republishes by starting/proving the target and stopping its peer —
	// never by rewriting the fragment.
	Pair bool `json:"pair"`
}

// swapContainer is what the readiness proof reads about one container.
type swapContainer struct {
	Exists   bool
	Status   string
	Health   string
	ExitCode int
	Restarts int
	IP       string
}

// swapRouter is the part of the proxy a swap uses; tests replace it.
type swapRouter interface {
	UpstreamHost(deploymentID string) (string, string, bool, error)
	RetargetUpstream(ctx context.Context, deploymentID, to string) error
	// EnableSwapPeer upgrades a legacy fragment to the standby
	// pair — the one transitional reload; every later flip is container
	// lifecycle with the Caddyfile byte-identical.
	EnableSwapPeer(ctx context.Context, deploymentID string, spec proxy.SwapSpec) error
	// LiveUpstream is what the running proxy prefers, which the fragment on
	// disk can be ahead of (an agent stopped between the write and the
	// reload). On the pair form it is the preferred (app) slot.
	LiveUpstream(ctx context.Context, deploymentID string) (string, error)
}

// republishRoute is the recovery's route move. Legacy form: rewrite and
// reload, then confirm the running proxy really routes to `to`. Pair form
// (standby pair): traffic follows liveness — prove `to` and stop its peer; the
// fragment never changes and no reload runs.
func (d *Docker) republishRoute(ctx context.Context, router swapRouter, appDir, deploymentID, to string, rec readySwapRecord) error {
	if !rec.Pair {
		if err := router.RetargetUpstream(ctx, deploymentID, to); err != nil {
			return err
		}
		live, err := router.LiveUpstream(ctx, deploymentID)
		if err != nil {
			return err
		}
		if live != to {
			return fmt.Errorf("the running proxy still routes to %s", strings.TrimPrefix(live, deploymentID+"-"))
		}
		return nil
	}
	id := rec.DeploymentID
	if to == id+"-app" {
		// Only bring `app` up when it is not running: `up -d` would recreate
		// a healthy previous version out of the swap file (which may carry
		// the NEW model) and turn a recovered_previous into a version swap.
		if c, err := d.swapInspect(ctx, id+"-app"); err != nil || !c.Exists || c.Status != "running" {
			upCtx, cancel := context.WithTimeout(ctx, composeUpTimeout)
			out, err := d.swapCompose(upCtx, appDir, readySwapComposeName, "up", "-d", "--no-build", "--pull", "never", "--no-deps", "app")
			cancel()
			if err != nil {
				return errors.New("the app container did not come back: " + tail(out, 256))
			}
		}
		if err := d.awaitSwapReady(ctx, id+"-app", rec.Port, rec.Host, rec.Policy); err != nil {
			return err
		}
		if out, err := d.swapCompose(ctx, appDir, readySwapComposeName, "stop", "app-next"); err != nil {
			return errors.New("the previous copy could not be stopped: " + tail(out, 256))
		}
		return nil
	}
	if err := d.awaitSwapReady(ctx, id+"-app-next", rec.Port, rec.Host, rec.Policy); err != nil {
		return err
	}
	if out, err := d.swapCompose(ctx, appDir, readySwapComposeName, "stop", "app"); err != nil {
		return errors.New("the app container could not be stopped: " + tail(out, 256))
	}
	return nil
}

// validateReadySwapPolicy rejects anything outside the contract.
func validateReadySwapPolicy(p *sdkclient.ReadySwapPolicy) error {
	if p == nil {
		return nil
	}
	if !readySwapPathPattern.MatchString(p.Path) || strings.Contains(p.Path, "..") {
		return errors.New("ready swap: invalid readiness path")
	}
	if p.StatusMin < 100 || p.StatusMax > 599 || p.StatusMin > p.StatusMax {
		return errors.New("ready swap: invalid accepted status range")
	}
	if p.TimeoutSeconds < 30 || p.TimeoutSeconds > 600 {
		return errors.New("ready swap: timeout must be 30-600 seconds")
	}
	if p.DrainSeconds < 0 || p.DrainSeconds > 60 {
		return errors.New("ready swap: drain must be 0-60 seconds")
	}
	return nil
}

func (d *Docker) swapRouter() swapRouter {
	if d.readySwapRouter != nil {
		return d.readySwapRouter
	}
	if d.Proxy == nil {
		return nil
	}
	return d.Proxy
}

func (d *Docker) swapCompose(ctx context.Context, appDir, file string, args ...string) ([]byte, error) {
	if d.readySwapCompose != nil {
		return d.readySwapCompose(ctx, appDir, file, args...)
	}
	project, err := composeProject(appDir)
	if err != nil {
		return []byte(err.Error()), err
	}
	cmd := d.dockerCmd(ctx, append([]string{"compose", "-p", project, "-f", filepath.Join(appDir, file)}, args...)...)
	cmd.Dir = appDir
	return cmd.CombinedOutput()
}

func (d *Docker) swapInspect(ctx context.Context, name string) (swapContainer, error) {
	if d.readySwapInspect != nil {
		return d.readySwapInspect(ctx, name)
	}
	out, err := d.dockerCmd(ctx, "container", "ls", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}").Output()
	if err != nil {
		return swapContainer{}, err
	}
	if strings.TrimSpace(string(out)) == "" {
		return swapContainer{}, nil
	}
	raw, err := d.dockerCmd(ctx, "inspect", "--format", "{{json .State}}|{{.RestartCount}}|{{with index .NetworkSettings.Networks \""+proxy.NetworkName+"\"}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil {
		return swapContainer{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(raw)), "|", 3)
	if len(parts) != 3 {
		return swapContainer{}, errors.New("unexpected container inspection")
	}
	var state struct {
		Status   string
		ExitCode int
		Health   *struct{ Status string }
	}
	if err := json.Unmarshal([]byte(parts[0]), &state); err != nil {
		return swapContainer{}, err
	}
	c := swapContainer{Exists: true, Status: state.Status, ExitCode: state.ExitCode, IP: parts[2]}
	if state.Health != nil {
		c.Health = state.Health.Status
	}
	c.Restarts, _ = strconv.Atoi(parts[1])
	return c, nil
}

func (d *Docker) swapProbe(ctx context.Context, url, host string) (int, error) {
	if d.readySwapProbe != nil {
		return d.readySwapProbe(ctx, url, host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Host = host
	req.Header.Set("User-Agent", "impreza-agent-ready-swap")
	client := &http.Client{
		Timeout: 5 * time.Second,
		// Never follow: a redirect answer is the app's answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// Direct to the container: no proxy from the environment, no reuse.
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (d *Docker) swapSleep(ctx context.Context, wait time.Duration) bool {
	if d.readySwapSleep != nil {
		return d.readySwapSleep(ctx, wait)
	}
	return sleepContext(ctx, wait)
}

func sleepContext(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// awaitSwapReady proves one container ready, or says why it is not.
func (d *Docker) awaitSwapReady(ctx context.Context, name, port, host string, policy sdkclient.ReadySwapPolicy) error {
	deadline := time.Now().Add(time.Duration(policy.TimeoutSeconds) * time.Second)
	if d.readySwapClock != nil {
		deadline = d.readySwapClock().Add(time.Duration(policy.TimeoutSeconds) * time.Second)
	}
	now := func() time.Time {
		if d.readySwapClock != nil {
			return d.readySwapClock()
		}
		return time.Now()
	}
	passes, baseline, last := 0, -1, "no answer yet"
	for now().Before(deadline) {
		c, err := d.swapInspect(ctx, name)
		switch {
		case err != nil:
			// The passes must be consecutive: an inspection error breaks the
			// sequence like any other failed check.
			last = "container inspection failed"
			passes = 0
		case !c.Exists:
			return errors.New("the new container disappeared")
		case c.Status == "exited" || c.Status == "dead":
			return fmt.Errorf("the new container stopped (exit code %d)", c.ExitCode)
		case c.Health == "unhealthy":
			return errors.New("the new container's healthcheck reports unhealthy")
		default:
			if baseline < 0 {
				baseline = c.Restarts
			}
			if c.Restarts-baseline >= crashLoopRestarts {
				return fmt.Errorf("the new container restarted %d times", c.Restarts-baseline)
			}
			if c.Status != "running" || (c.Health != "" && c.Health != "healthy") {
				last = "container " + c.Status + healthSuffix(c.Health)
				passes = 0
				break
			}
			ip := net.ParseIP(c.IP)
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				last = "no address on the proxy network"
				passes = 0
				break
			}
			url := "http://" + net.JoinHostPort(ip.String(), port) + policy.Path
			code, err := d.swapProbe(ctx, url, host)
			if err != nil {
				last = "HTTP request failed"
				passes = 0
				break
			}
			if code < policy.StatusMin || code > policy.StatusMax {
				last = fmt.Sprintf("HTTP %d (accepted %d-%d)", code, policy.StatusMin, policy.StatusMax)
				passes = 0
				break
			}
			passes++
			if passes >= readySwapPasses {
				return nil
			}
		}
		if !d.swapSleep(ctx, time.Second) {
			return errors.New("readiness check cancelled")
		}
	}
	return fmt.Errorf("not ready within %d seconds: %s", policy.TimeoutSeconds, last)
}

// statusClassOf maps the policy's minimum accepted status to the class
// Caddy's health_status gate understands (one class only; the swap's own
// readiness proof keeps the policy's full range).
func statusClassOf(min int) string {
	return fmt.Sprintf("%dxx", min/100)
}

// swapSpecFor attaches the standby pair to a route when the payload
// asks for ready swaps and the route's upstream is the deployment's app
// container — the only shape a swap flips. Other upstreams render exactly
// as before.
func swapSpecFor(p sdkclient.DeployPayload, upstream string) *proxy.SwapSpec {
	if p.ReadySwap == nil || validateReadySwapPolicy(p.ReadySwap) != nil {
		return nil
	}
	host, port, ok := strings.Cut(upstream, ":")
	if !ok || host != p.DeploymentID+"-app" {
		return nil
	}
	return &proxy.SwapSpec{
		Peer:        p.DeploymentID + "-app-next:" + port,
		Path:        p.ReadySwap.Path,
		StatusClass: statusClassOf(p.ReadySwap.StatusMin),
	}
}

// swapSpecCarried re-attaches a pair read off the existing fragment, but
// only to the app container's own route — any other upstream keeps the
// single form.
func swapSpecCarried(spec *proxy.SwapSpec, deploymentID, upstream string) *proxy.SwapSpec {
	if spec == nil {
		return nil
	}
	host, _, ok := strings.Cut(upstream, ":")
	if !ok || host != deploymentID+"-app" {
		return nil
	}
	return spec
}

func healthSuffix(h string) string {
	if h == "" {
		return ""
	}
	return " (" + h + ")"
}

// readySwapEligibility checks the new resolved Compose model and the routes.
// The reasons are the ones the API gives, in English.
func readySwapEligibility(p sdkclient.DeployPayload, resolved []byte) error {
	if p.Manifest.Runtime.TorEgress || p.Manifest.Runtime.Sandbox != nil {
		return errors.New("Tor egress and sandboxed apps are not eligible for zero-downtime redeploys")
	}
	if len(p.Manifest.Runtime.ServiceBindingRetirements) != 0 || p.Manifest.Runtime.ServiceBindingRotation != nil || p.Manifest.Runtime.RestoreDatabase != nil || p.Quiesce != nil {
		return errors.New("a database connection change or restore is in progress; redeploy normally")
	}
	if len(p.Routes) == 0 {
		return errors.New("the app has no route through the proxy (no domain or onion)")
	}
	var model struct {
		Services map[string]map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(resolved, &model); err != nil {
		return errors.New("the Compose file could not be read")
	}
	if len(model.Services) != 1 || model.Services["app"] == nil {
		return errors.New("only single-service apps (one service named app) are eligible")
	}
	app := model.Services["app"]
	var name string
	if json.Unmarshal(app["container_name"], &name) != nil || name != p.DeploymentID+"-app" {
		return errors.New("the app container is not named for this deployment")
	}
	if raw, ok := app["ports"]; ok && string(raw) != "null" && string(raw) != "[]" {
		return errors.New("the app publishes a port on the host; a second copy cannot bind it")
	}
	if _, ok := app["network_mode"]; ok {
		return errors.New("the app uses a custom network mode")
	}
	var networks map[string]json.RawMessage
	if json.Unmarshal(app["networks"], &networks) != nil || networks[proxy.NetworkName] == nil {
		return errors.New("the app is not attached to the proxy network")
	}
	if raw, ok := app["volumes"]; ok && string(raw) != "null" {
		var volumes []struct {
			Type     string `json:"type"`
			ReadOnly bool   `json:"read_only"`
		}
		if json.Unmarshal(raw, &volumes) != nil {
			return errors.New("the app's volumes could not be read")
		}
		for _, v := range volumes {
			if v.Type != "tmpfs" && !v.ReadOnly {
				return errors.New("the app has a writable volume; two copies writing the same data is not safe")
			}
		}
	}
	return nil
}

// readySwapModel adds `app-next`, a copy of `app` under its own container
// name, to the resolved model. The file carries resolved environment values:
// 0600 in the root-owned app directory, removed when the swap ends.
func readySwapModel(resolved []byte, deploymentID string) ([]byte, error) {
	var model map[string]any
	if err := json.Unmarshal(resolved, &model); err != nil {
		return nil, err
	}
	services, _ := model["services"].(map[string]any)
	app, _ := services["app"].(map[string]any)
	if app == nil {
		return nil, errors.New("no app service")
	}
	copyRaw, err := json.Marshal(app)
	if err != nil {
		return nil, err
	}
	var next map[string]any
	if err := json.Unmarshal(copyRaw, &next); err != nil {
		return nil, err
	}
	next["container_name"] = deploymentID + "-app-next"
	delete(next, "ports")
	delete(next, "hostname")
	services["app-next"] = next
	return json.Marshal(model)
}

func (d *Docker) writeSwapJournal(appDir string, r readySwapRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(appDir, readySwapJournalName), raw, 0600)
}

func readSwapJournal(appDir string) (*readySwapRecord, error) {
	raw, err := os.ReadFile(filepath.Join(appDir, readySwapJournalName))
	if err != nil {
		return nil, err
	}
	var r readySwapRecord
	if err := json.Unmarshal(raw, &r); err != nil || r.Version != 1 || !recoveryDeploymentID.MatchString(r.DeploymentID) {
		return nil, errors.New("ready swap journal unreadable")
	}
	return &r, nil
}

func (d *Docker) clearSwapFiles(appDir string) error {
	var failures []error
	for _, name := range []string{readySwapComposeName, readySwapJournalName} {
		if err := os.Remove(filepath.Join(appDir, name)); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// writeReleaseFiles puts a release's configuration back without touching
// containers: the release's own container is still the one running.
func writeReleaseFiles(dir string, previous *runtimeRelease) error {
	if err := writeStartupPolicy(dir, previous.Startup); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "compose.yaml"), previous.Compose, 0600); err != nil {
		return err
	}
	if previous.EnvExists {
		return writeAtomic(filepath.Join(dir, ".env"), previous.Env, 0600)
	}
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func swapProbeHost(p sdkclient.DeployPayload) string {
	for _, r := range p.Routes {
		if r.Hostname != "" {
			return r.Hostname
		}
	}
	if host := envValue(p.Vars, "DOMAIN"); host != "" {
		return host
	}
	return "localhost"
}

// readySwap runs the swap. handled=false means "not a swap: replace normally"
// (the running version was not healthy, so there was nothing to keep).
func (d *Docker) readySwap(ctx context.Context, cmd *sdkclient.PollCommand, p sdkclient.DeployPayload, previous *runtimeRelease, workID string) (result sdkclient.DeployResult, report *sdkclient.ReadySwapReport, handled bool) {
	appDir := d.appDir(p.DeploymentID)
	refuse := func(reason string) (sdkclient.DeployResult, *sdkclient.ReadySwapReport, bool) {
		rep := &sdkclient.ReadySwapReport{Outcome: "refused", Phase: "", Reason: reason}
		r := failResult(cmd.ID, "Zero-downtime redeploy refused: "+reason+". Nothing was changed; the running version keeps serving.")
		if previous != nil {
			if err := writeReleaseFiles(appDir, previous); err != nil {
				r.Error += "\nThe previous configuration files could not be put back: " + err.Error()
			}
			r.Rollback = &sdkclient.DeploymentRollback{Status: "restored", ReleaseID: previous.Metadata.ID, RuntimeState: "healthy"}
			r.Release = &previous.Metadata
		}
		r.DeploymentID = p.DeploymentID
		r.ReadySwap = rep
		return r, rep, true
	}
	if err := validateReadySwapPolicy(p.ReadySwap); err != nil {
		return refuse(err.Error())
	}
	if previous == nil {
		return sdkclient.DeployResult{}, &sdkclient.ReadySwapReport{Outcome: "not_used", Reason: "the running version was not healthy, so there was nothing to keep serving; replaced normally"}, false
	}
	router := d.swapRouter()
	if router == nil {
		return refuse("the proxy is not managed by this agent")
	}
	qctx, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	resolved, err := d.swapCompose(qctx, appDir, composeFileName, "config", "--format", "json")
	cancel()
	if err != nil {
		return refuse("the Compose file could not be resolved")
	}
	if err := readySwapEligibility(p, resolved); err != nil {
		return refuse(err.Error())
	}
	host, port, pair, err := router.UpstreamHost(p.DeploymentID)
	if err != nil || host != p.DeploymentID+"-app" {
		return refuse("the current route does not point at the app container")
	}
	if live, err := router.LiveUpstream(ctx, p.DeploymentID); err != nil || live != p.DeploymentID+"-app" {
		return refuse("the running proxy does not route to the app container")
	}
	if !pair {
		// The legacy single-upstream fragment cannot flip without a
		// reload (the reload is the request-killing server swap). Upgrade
		// it to the standby pair first — the one transitional reload — so
		// this and every later swap run reload-free. Nothing container-side
		// has been touched yet; a refusal here leaves the previous version
		// serving exactly as before.
		spec := proxy.SwapSpec{
			Peer:        p.DeploymentID + "-app-next:" + port,
			Path:        p.ReadySwap.Path,
			StatusClass: statusClassOf(p.ReadySwap.StatusMin),
		}
		if err := router.EnableSwapPeer(ctx, p.DeploymentID, spec); err != nil {
			return refuse("the routing fragment could not carry the standby slot: " + err.Error())
		}
		pair = true
	}
	model, err := readySwapModel(resolved, p.DeploymentID)
	if err != nil {
		return refuse("the Compose file could not be prepared")
	}
	if err := writeAtomic(filepath.Join(appDir, readySwapComposeName), model, 0600); err != nil {
		return refuse("the swap file could not be written")
	}
	rec := readySwapRecord{Version: 1, DeploymentID: p.DeploymentID, CommandID: cmd.ID, WorkID: workID, ReleaseID: previous.Metadata.ID, Port: port, Host: swapProbeHost(p), Policy: *p.ReadySwap, Pair: pair}
	r, rep := d.runReadySwap(ctx, cmd, appDir, rec, previous)
	return r, rep, true
}

func (d *Docker) swapPhase(appDir string, rec *readySwapRecord, phase string) error {
	rec.Phase = phase
	return d.writeSwapJournal(appDir, *rec)
}

func (d *Docker) runReadySwap(ctx context.Context, cmd *sdkclient.PollCommand, appDir string, rec readySwapRecord, previous *runtimeRelease) (sdkclient.DeployResult, *sdkclient.ReadySwapReport) {
	id := rec.DeploymentID
	router := d.swapRouter()
	up := []string{"up", "-d", "--no-build", "--pull", "never", "--no-deps"}
	fail := func(phase, reason string) (sdkclient.DeployResult, *sdkclient.ReadySwapReport) {
		// The new version never took traffic from the running one.
		rep := &sdkclient.ReadySwapReport{Outcome: "kept_previous", Phase: phase, Reason: reason}
		r := failResult(cmd.ID, "Zero-downtime redeploy: the new version was not ready ("+reason+"). The previous version kept serving.")
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), composeUpTimeout)
		defer cancel()
		logs, _ := d.swapCompose(cleanup, appDir, readySwapComposeName, "logs", "--tail=40", "--no-color", "app-next")
		if out, err := d.swapCompose(cleanup, appDir, readySwapComposeName, "rm", "-s", "-f", "app-next"); err != nil {
			r.Error += "\nThe unused new container could not be removed: " + tail(out, 512)
			rep.Outcome = "kept_previous_cleanup_pending"
		}
		if err := writeReleaseFiles(appDir, previous); err != nil {
			r.Error += "\nThe previous configuration files could not be put back: " + err.Error()
		}
		if rep.Outcome == "kept_previous" {
			if err := d.clearSwapFiles(appDir); err != nil {
				r.Error += "\nSwap files retained: " + err.Error()
			}
		}
		if len(logs) > 0 {
			r.Error += "\n--- new container logs ---\n" + tail(logs, 2048)
		}
		r.DeploymentID = id
		r.Rollback = &sdkclient.DeploymentRollback{Status: "restored", ReleaseID: previous.Metadata.ID, RuntimeState: "healthy"}
		r.Release = &previous.Metadata
		r.ReadySwap = rep
		return r, rep
	}

	// 1. The new version next to the running one.
	if err := d.swapPhase(appDir, &rec, swapStartingNext); err != nil {
		return fail(swapStartingNext, "the swap journal could not be written")
	}
	d.deploymentProgress(ctx, cmd, "replacing")
	upCtx, cancelUp := context.WithTimeout(ctx, composeUpTimeout)
	out, err := d.swapCompose(upCtx, appDir, readySwapComposeName, append(up, "app-next")...)
	cancelUp()
	if err != nil {
		return fail(swapStartingNext, "the new container did not start: "+tail(out, 512))
	}
	d.deploymentProgress(ctx, cmd, "checking_readiness")
	if err := d.awaitSwapReady(ctx, id+"-app-next", rec.Port, rec.Host, rec.Policy); err != nil {
		return fail(swapStartingNext, err.Error())
	}

	// 2. The traffic flip. Pair form: stop the serving container —
	// Caddy's active check benches it within its interval and dials retry
	// on the standby; the in-flight requests drain on the container's own
	// stop grace. Legacy form: the route moves by rewrite + reload (the
	// known one-request loss, kept for fragments not yet upgraded).
	if err := d.swapPhase(appDir, &rec, swapRoutedNext); err != nil {
		return fail(swapStartingNext, "the swap journal could not be written")
	}
	d.deploymentProgress(ctx, cmd, "routing")
	if rec.Pair {
		stopCtx, cancelStop := context.WithTimeout(ctx, composeUpTimeout)
		out, err := d.swapCompose(stopCtx, appDir, readySwapComposeName, "stop", "app")
		cancelStop()
		if err != nil {
			return fail(swapRoutedNext, "the serving container could not be stopped: "+tail(out, 256))
		}
	} else if err := router.RetargetUpstream(ctx, id, id+"-app-next"); err != nil {
		// RetargetUpstream restores the previous fragment on a rejected reload.
		return fail(swapRoutedNext, "the proxy did not accept the new route")
	}
	d.swapSleep(ctx, time.Duration(rec.Policy.DrainSeconds)*time.Second)

	// 3. The canonical container gets the new version, with no traffic.
	if err := d.swapPhase(appDir, &rec, swapReplacingApp); err != nil {
		return d.settleReadySwap(context.WithoutCancel(ctx), cmd, appDir, rec, previous, "the swap journal could not be written")
	}
	d.deploymentProgress(ctx, cmd, "replacing")
	return d.finishReadySwap(ctx, cmd, appDir, rec, previous, "")
}

// finishReadySwap runs phases 3-4 from replacing_app, in the swap and in
// recovery alike: prove the new `app`, move the route back, remove next.
func (d *Docker) finishReadySwap(ctx context.Context, cmd *sdkclient.PollCommand, appDir string, rec readySwapRecord, previous *runtimeRelease, recovered string) (sdkclient.DeployResult, *sdkclient.ReadySwapReport) {
	id := rec.DeploymentID
	router := d.swapRouter()
	upCtx, cancelUp := context.WithTimeout(ctx, composeUpTimeout)
	out, err := d.swapCompose(upCtx, appDir, readySwapComposeName, "up", "-d", "--no-build", "--pull", "never", "--no-deps", "app")
	cancelUp()
	if err == nil {
		d.deploymentProgress(ctx, cmd, "checking_readiness")
		err = d.awaitSwapReady(ctx, id+"-app", rec.Port, rec.Host, rec.Policy)
	} else {
		err = errors.New("the app container was not recreated: " + tail(out, 512))
	}
	if err != nil {
		return d.settleReadySwap(context.WithoutCancel(ctx), cmd, appDir, rec, previous, err.Error())
	}
	if err := d.swapPhase(appDir, &rec, swapRoutedApp); err != nil {
		return d.settleReadySwap(context.WithoutCancel(ctx), cmd, appDir, rec, previous, "the swap journal could not be written")
	}
	d.deploymentProgress(ctx, cmd, "routing")
	if !rec.Pair {
		if err := router.RetargetUpstream(ctx, id, id+"-app"); err != nil {
			return d.settleReadySwap(context.WithoutCancel(ctx), cmd, appDir, rec, previous, "the proxy did not accept the route back to the app container")
		}
	} else {
		// Standby pair: the app slot takes traffic back by its own health —
		// awaitSwapReady proved it; one interval of headroom lets Caddy's
		// active check converge before the standby is removed.
		if !d.swapSleep(ctx, time.Second) {
			return d.settleReadySwap(context.WithoutCancel(ctx), cmd, appDir, rec, previous, "the readiness check cancelled")
		}
	}
	return d.completeReadySwap(ctx, cmd, appDir, rec, recovered)
}

// completeReadySwap: the new `app` serves; drain and remove `-app-next`.
func (d *Docker) completeReadySwap(ctx context.Context, cmd *sdkclient.PollCommand, appDir string, rec readySwapRecord, recovered string) (sdkclient.DeployResult, *sdkclient.ReadySwapReport) {
	d.swapSleep(ctx, time.Duration(rec.Policy.DrainSeconds)*time.Second)
	outcome := "switched"
	if recovered != "" {
		outcome = "recovered_new"
	}
	rep := &sdkclient.ReadySwapReport{Outcome: outcome, Phase: swapDone, Reason: recovered}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), composeUpTimeout)
	defer cancel()
	if out, err := d.swapCompose(cleanup, appDir, readySwapComposeName, "rm", "-s", "-f", "app-next"); err != nil {
		rep.Reason = strings.TrimSpace(rep.Reason + "; the previous copy could not be removed: " + tail(out, 256))
		rep.Phase = swapRoutedApp
		_ = d.writeSwapJournal(appDir, rec)
	} else if err := d.clearSwapFiles(appDir); err != nil {
		rep.Reason = strings.TrimSpace(rep.Reason + "; swap files retained")
	}
	r := sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: rec.DeploymentID, ReadySwap: rep}
	return r, rep
}

// settleReadySwap is the one exit for a failure after the route may have
// moved. It leaves exactly one version serving:
//   - the route is on `app` and `app` is proven: complete (new or previous);
//   - otherwise, `-app-next` (the new version) still proven: the previous
//     release goes back into `app`, is proven, takes the route, and next is
//     removed; if the previous release cannot be proven, the route stays on
//     next (the new version serves) and `app` is stopped.
func (d *Docker) settleReadySwap(ctx context.Context, cmd *sdkclient.PollCommand, appDir string, rec readySwapRecord, previous *runtimeRelease, reason string) (sdkclient.DeployResult, *sdkclient.ReadySwapReport) {
	id := rec.DeploymentID
	router := d.swapRouter()
	r := failResult(cmd.ID, "Zero-downtime redeploy did not complete: "+reason+".")
	r.DeploymentID = id
	report := func(outcome, phase string) (sdkclient.DeployResult, *sdkclient.ReadySwapReport) {
		rep := &sdkclient.ReadySwapReport{Outcome: outcome, Phase: phase, Reason: reason}
		r.ReadySwap = rep
		return r, rep
	}
	if previous == nil {
		return report("recovery_required", rec.Phase)
	}
	// Put the previous release into `app` next to the serving new copy.
	if err := writeReleaseFiles(appDir, previous); err != nil {
		r.Error += "\nThe previous configuration could not be written: " + err.Error()
		return d.serveNext(ctx, appDir, rec, &r, reason)
	}
	prevModel, err := readySwapModelFromRelease(previous, filepath.Join(appDir, readySwapComposeName), id)
	if err == nil {
		err = writeAtomic(filepath.Join(appDir, readySwapComposeName), prevModel, 0600)
	}
	if err == nil {
		upCtx, cancel := context.WithTimeout(ctx, composeUpTimeout)
		var out []byte
		out, err = d.swapCompose(upCtx, appDir, readySwapComposeName, "up", "-d", "--no-build", "--pull", "never", "--no-deps", "app")
		cancel()
		if err != nil {
			err = errors.New(tail(out, 256))
		}
	}
	if err == nil {
		err = d.awaitSwapReady(ctx, id+"-app", rec.Port, rec.Host, rec.Policy)
	}
	if err == nil {
		err = d.republishRoute(ctx, router, appDir, id, id+"-app", rec)
	}
	if err != nil {
		r.Error += "\nThe previous version could not be brought back: " + err.Error()
		return d.serveNext(ctx, appDir, rec, &r, reason)
	}
	d.swapSleep(ctx, time.Duration(rec.Policy.DrainSeconds)*time.Second)
	if out, err := d.swapCompose(ctx, appDir, readySwapComposeName, "rm", "-s", "-f", "app-next"); err != nil {
		r.Error += "\nThe new copy could not be removed: " + tail(out, 256)
		return report("recovered_previous_cleanup_pending", rec.Phase)
	}
	_ = d.clearSwapFiles(appDir)
	r.Error += "\nThe previous version serves again."
	r.Rollback = &sdkclient.DeploymentRollback{Status: "restored", ReleaseID: previous.Metadata.ID, RuntimeState: "healthy"}
	r.Release = &previous.Metadata
	return report("recovered_previous", rec.Phase)
}

// serveNext is the last resort: the new version keeps the route on
// `-app-next`, and `app` is stopped so only one copy runs.
func (d *Docker) serveNext(ctx context.Context, appDir string, rec readySwapRecord, r *sdkclient.DeployResult, reason string) (sdkclient.DeployResult, *sdkclient.ReadySwapReport) {
	id := rec.DeploymentID
	router := d.swapRouter()
	if err := d.republishRoute(ctx, router, appDir, id, id+"-app-next", rec); err != nil {
		r.Error += "\nThe route could not be confirmed on the new copy: " + err.Error()
	}
	_, _ = d.swapCompose(ctx, appDir, readySwapComposeName, "stop", "app")
	rep := &sdkclient.ReadySwapReport{Outcome: "serving_next", Phase: rec.Phase, Reason: reason}
	r.Error += "\nThe new version serves from its temporary container; redeploy to return to the normal layout."
	r.ReadySwap = rep
	return *r, rep
}

// readySwapModelFromRelease keeps the current `app-next` (the new version)
// and puts the previous release's `app` definition beside it.
func readySwapModelFromRelease(previous *runtimeRelease, current, deploymentID string) ([]byte, error) {
	raw, err := os.ReadFile(current)
	if err != nil {
		return nil, err
	}
	var cur, prev map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(previous.Compose, &prev); err != nil {
		return nil, err
	}
	curServices, _ := cur["services"].(map[string]any)
	prevServices, _ := prev["services"].(map[string]any)
	if curServices == nil || prevServices == nil || curServices["app-next"] == nil || prevServices["app"] == nil {
		return nil, errors.New("swap model incomplete")
	}
	prevServices["app-next"] = curServices["app-next"]
	return json.Marshal(prev)
}

// RecoverReadySwap settles an interrupted swap found on disk (the worker or
// the agent stopped mid-swap). It returns nil when there is nothing to do.
// The caller guarantees no swap process for this deployment is alive.
func (d *Docker) RecoverReadySwap(ctx context.Context, deploymentID string) *sdkclient.DeployResult {
	appDir := d.appDir(deploymentID)
	rec, err := readSwapJournal(appDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	cmd := &sdkclient.PollCommand{ID: "", Kind: sdkclient.CommandDeploy}
	if err != nil {
		r := failResult("", "Zero-downtime redeploy recovery: the swap journal is unreadable; review required.")
		return &r
	}
	cmd.ID = rec.CommandID
	previous, err := loadRelease(appDir, rec.ReleaseID)
	if err != nil {
		previous = nil
	}
	router := d.swapRouter()
	if router == nil {
		r := failResult(rec.CommandID, "Zero-downtime redeploy recovery: the proxy is not managed by this agent.")
		return &r
	}
	d.Log.Warn("ready swap: settling an interrupted swap", "deployment_id", deploymentID, "phase", rec.Phase)
	var result sdkclient.DeployResult
	switch rec.Phase {
	case swapStartingNext, swapRoutedNext:
		// The previous version still runs in `app`: it keeps (or gets back)
		// the route once proven; the new copy goes.
		err := d.awaitSwapReady(ctx, deploymentID+"-app", rec.Port, rec.Host, rec.Policy)
		if err == nil {
			err = d.republishRoute(ctx, router, appDir, deploymentID, deploymentID+"-app", *rec)
		}
		if err != nil {
			// The previous version is not answering: prefer the new copy if it is.
			if nerr := d.awaitSwapReady(ctx, deploymentID+"-app-next", rec.Port, rec.Host, rec.Policy); nerr == nil {
				result, _ = d.finishReadySwap(ctx, cmd, appDir, *rec, previous, "interrupted swap settled on the new version (the previous one was not answering)")
				break
			}
			r := failResult(rec.CommandID, "Zero-downtime redeploy recovery: neither version answered; review required.")
			r.ReadySwap = &sdkclient.ReadySwapReport{Outcome: "recovery_required", Phase: rec.Phase, Reason: err.Error()}
			result = r
			break
		}
		d.swapSleep(ctx, time.Duration(rec.Policy.DrainSeconds)*time.Second)
		r := failResult(rec.CommandID, "Zero-downtime redeploy was interrupted before the new version took over; the previous version serves.")
		out, rmErr := d.swapCompose(ctx, appDir, readySwapComposeName, "rm", "-s", "-f", "app-next")
		if rmErr != nil {
			r.Error += "\nThe new copy could not be removed: " + tail(out, 256)
		}
		if previous != nil {
			if err := writeReleaseFiles(appDir, previous); err != nil {
				r.Error += "\nThe previous configuration files could not be put back: " + err.Error()
			}
			r.Rollback = &sdkclient.DeploymentRollback{Status: "restored", ReleaseID: previous.Metadata.ID, RuntimeState: "healthy"}
			r.Release = &previous.Metadata
		}
		if rmErr == nil {
			_ = d.clearSwapFiles(appDir)
		}
		r.ReadySwap = &sdkclient.ReadySwapReport{Outcome: "recovered_previous", Phase: rec.Phase, Reason: "interrupted"}
		result = r
	case swapReplacingApp:
		result, _ = d.finishReadySwap(ctx, cmd, appDir, *rec, previous, "interrupted swap settled on the new version")
	case swapRoutedApp:
		// The new version is in `app` and the route was moving back to it.
		// Success needs `app` answering AND the running proxy on it; until
		// then `-app-next` is the copy that may be serving and stays.
		if err := d.awaitSwapReady(ctx, deploymentID+"-app", rec.Port, rec.Host, rec.Policy); err != nil {
			result, _ = d.settleReadySwap(ctx, cmd, appDir, *rec, previous, "the app container did not answer after the interruption: "+err.Error())
			break
		}
		if err := d.republishRoute(ctx, router, appDir, deploymentID, deploymentID+"-app", *rec); err != nil {
			result, _ = d.settleReadySwap(ctx, cmd, appDir, *rec, previous, "the route back to the app container was not confirmed: "+err.Error())
			break
		}
		result, _ = d.completeReadySwap(ctx, cmd, appDir, *rec, "interrupted swap settled on the new version")
	default:
		r := failResult(rec.CommandID, "Zero-downtime redeploy recovery: unknown phase; review required.")
		result = r
	}
	result.CommandID = rec.CommandID
	result.DeploymentID = deploymentID
	return &result
}

// RecoverInterruptedSwaps runs at agent start, before any command: a swap
// the agent itself was running (synchronous replacement, no worker) cannot
// still be alive, so it is settled now. Swaps owned by a supervised worker
// are left to the worker, or to recoverSwapOfDeadWorker once it is gone.
func (d *Docker) RecoverInterruptedSwaps(ctx context.Context) {
	journals, err := filepath.Glob(filepath.Join(d.StateDir, "apps", "*", readySwapJournalName))
	if err != nil {
		return
	}
	for _, path := range journals {
		id := filepath.Base(filepath.Dir(path))
		if !recoveryDeploymentID.MatchString(id) {
			continue
		}
		rec, err := readSwapJournal(filepath.Dir(path))
		if err != nil || rec.DeploymentID != id || rec.WorkID != "" {
			if err != nil {
				d.Log.Warn("ready swap: unreadable journal left for review", "deployment_id", id)
			}
			continue
		}
		if r := d.RecoverReadySwap(ctx, id); r != nil {
			outcome := ""
			if r.ReadySwap != nil {
				outcome = r.ReadySwap.Outcome
			}
			d.Log.Warn("ready swap: interrupted swap settled at startup", "deployment_id", id, "status", r.Status, "outcome", outcome)
		}
	}
}
