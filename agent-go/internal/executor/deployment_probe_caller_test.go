package executor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Observe the probe outcome through the caller, finishReplacement,
// on any OS. The docker CLI is this test binary itself, copied as "docker"
// onto PATH and switched by an environment variable: it records each call,
// prints nothing (no containers, so the settle gate is skipped as advisory)
// and fails only the subcommand the case names. The proxy is the real
// proxy.Caddy over a temp state dir; only the network probe is pinned by
// the probeHTTPS seam.
const deploymentProbeFakeDockerEnv = "DEPLOYMENT_PROBE_FAKE_DOCKER"

func init() {
	if os.Getenv(deploymentProbeFakeDockerEnv) == "" {
		return
	}
	args := strings.Join(os.Args[1:], " ")
	if log := os.Getenv("DEPLOYMENT_PROBE_DOCKER_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = io.WriteString(f, args+"\n")
			_ = f.Close()
		}
	}
	if fail := os.Getenv("DEPLOYMENT_PROBE_DOCKER_FAIL"); fail != "" && len(os.Args) > 1 && os.Args[1] == fail {
		os.Exit(1)
	}
	os.Exit(0)
}

type deploymentProbeCase struct {
	routes      []sdkclient.Route
	onion       string
	failDocker  string
	probeResult bool
}

type deploymentProbeOutcome struct {
	result sdkclient.DeployResult
	calls  []string
	docker string
}

func deploymentProbeRun(t *testing.T, c deploymentProbeCase) deploymentProbeOutcome {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "docker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), raw, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerLog := filepath.Join(root, "docker.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(deploymentProbeFakeDockerEnv, "1")
	t.Setenv("DEPLOYMENT_PROBE_DOCKER_LOG", dockerLog)
	t.Setenv("DEPLOYMENT_PROBE_DOCKER_FAIL", c.failDocker)

	var calls []string
	orig := probeHTTPS
	t.Cleanup(func() { probeHTTPS = orig })
	probeHTTPS = func(ctx context.Context, host string, log *slog.Logger) bool {
		calls = append(calls, host)
		return c.probeResult
	}

	state := t.TempDir()
	d := &Docker{StateDir: state, Log: discardLogger(), Proxy: proxy.New(state, discardLogger())}
	id := "dpl_" + strings.Repeat("e", 16)
	dir := d.appDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  app:\n    image: example.invalid/app:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := sdkclient.DeployPayload{DeploymentID: id, Vars: map[string]any{"DEPLOYMENT_ID": id}, Routes: c.routes,
		Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ComposeYAML: "services: {}"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := d.finishReplacement(ctx, &sdkclient.PollCommand{ID: "cmd_deploymentProbe"}, p, nil, false, c.onion)
	logged, _ := os.ReadFile(dockerLog)
	return deploymentProbeOutcome{result: result, calls: calls, docker: string(logged)}
}

const deploymentProbeHost = "deploymentProbe.example.invalid"

func deploymentProbeClearnet() []sdkclient.Route {
	return []sdkclient.Route{{Hostname: deploymentProbeHost, Upstream: "dpl_eeeeeeeeeeeeeeee-app-1:80"}}
}

// The fake must really be in the path: compose up ran through it, so the
// result below is the caller's, not an early failure.
func deploymentProbeRequireCaller(t *testing.T, o deploymentProbeOutcome) {
	t.Helper()
	if !strings.Contains(o.docker, " up -d ") {
		t.Fatalf("finishReplacement did not reach compose up through the fake docker; calls:\n%s", o.docker)
	}
}

func TestDeploymentProbeCallerProbeConfirmed(t *testing.T) {
	o := deploymentProbeRun(t, deploymentProbeCase{routes: deploymentProbeClearnet(), probeResult: true})
	deploymentProbeRequireCaller(t, o)
	if o.result.Status != "success" {
		t.Fatalf("status %q, want success: %s", o.result.Status, o.result.LogsTail)
	}
	if len(o.calls) != 1 || o.calls[0] != deploymentProbeHost {
		t.Fatalf("probe calls %v, want exactly [%s]", o.calls, deploymentProbeHost)
	}
	if !strings.Contains(o.result.LogsTail, "public HTTPS confirmed on "+deploymentProbeHost) || strings.Contains(o.result.LogsTail, "not confirmed") {
		t.Fatalf("a passing probe must read as confirmed, never as not confirmed: %s", o.result.LogsTail)
	}
}

func TestDeploymentProbeCallerProbeNotConfirmedStaysSuccessAdvisory(t *testing.T) {
	o := deploymentProbeRun(t, deploymentProbeCase{routes: deploymentProbeClearnet(), probeResult: false})
	deploymentProbeRequireCaller(t, o)
	if o.result.Status != "success" {
		t.Fatalf("a failed probe must not fail the install (fail-open, BYO/slow start): status %q", o.result.Status)
	}
	if len(o.calls) != 1 {
		t.Fatalf("probe calls %v, want exactly one (no retry)", o.calls)
	}
	if !strings.Contains(o.result.LogsTail, "public HTTPS not confirmed on "+deploymentProbeHost) || !strings.Contains(o.result.LogsTail, "(advisory)") {
		t.Fatalf("the result lost the probe outcome: %s", o.result.LogsTail)
	}
	if strings.Contains(o.result.LogsTail, "public HTTPS confirmed") {
		t.Fatalf("a failed probe reads as confirmed: %s", o.result.LogsTail)
	}
	for _, destructive := range []string{" down", " rm ", " stop "} {
		if strings.Contains(o.docker, destructive) {
			t.Fatalf("a failed probe triggered %q: %s", destructive, o.docker)
		}
	}
}

func TestDeploymentProbeCallerOnionOnlyClaimsNothing(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	o := deploymentProbeRun(t, deploymentProbeCase{routes: []sdkclient.Route{{Upstream: "dpl_eeeeeeeeeeeeeeee-app-1:80", Onion: &sdkclient.RouteOnion{Enabled: true}}},
		onion: onion, probeResult: true})
	deploymentProbeRequireCaller(t, o)
	if o.result.Status != "success" {
		t.Fatalf("status %q: %s", o.result.Status, o.result.LogsTail)
	}
	if len(o.calls) != 0 {
		t.Fatalf("an onion-only deploy probed clearnet %v", o.calls)
	}
	if strings.Contains(o.result.LogsTail, "public HTTPS") {
		t.Fatalf("an onion-only deploy made a public HTTPS claim: %s", o.result.LogsTail)
	}
}

func TestDeploymentProbeCallerRouteApplyFailureNeverReadsVerified(t *testing.T) {
	o := deploymentProbeRun(t, deploymentProbeCase{routes: deploymentProbeClearnet(), failDocker: "exec", probeResult: false})
	deploymentProbeRequireCaller(t, o)
	if !strings.Contains(o.docker, "exec "+proxy.ContainerName+" caddy reload") {
		t.Fatalf("the route apply did not reach the proxy reload; calls:\n%s", o.docker)
	}
	if o.result.Status != "success" {
		t.Fatalf("a route apply failure must stay advisory here: status %q", o.result.Status)
	}
	if !strings.Contains(o.result.LogsTail, "public route apply failed (advisory)") {
		t.Fatalf("the route apply failure is missing from the result: %s", o.result.LogsTail)
	}
	if strings.Contains(o.result.LogsTail, "public HTTPS confirmed") {
		t.Fatalf("a failed route reads as verified HTTPS: %s", o.result.LogsTail)
	}
}

// No clearnet route at all: the route block does not run, nothing is probed
// or claimed (control, unchanged from the base).
func TestDeploymentProbeCallerNoRoutesClaimsNothing(t *testing.T) {
	o := deploymentProbeRun(t, deploymentProbeCase{probeResult: true})
	deploymentProbeRequireCaller(t, o)
	if len(o.calls) != 0 || strings.Contains(o.result.LogsTail, "public HTTPS") || strings.Contains(o.result.LogsTail, "public route") {
		t.Fatalf("a deploy without routes made a public claim (calls %v): %s", o.calls, o.result.LogsTail)
	}
}
