package executor

import (
	"context"
	"encoding/json"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// New assertions on the actual finishReplacement result, reusing the existing
// portable fake-docker transport. No real Docker, DNS, TLS or host call.
func TestDeploymentReadinessCallerReadiness(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	for _, tc := range []struct {
		name             string
		input            deploymentProbeCase
		endpoint, reason string
	}{
		{"confirmed", deploymentProbeCase{routes: deploymentProbeClearnet(), probeResult: true}, "CONFIRMED", "PROBE_PASSED"},
		{"unconfirmed", deploymentProbeCase{routes: deploymentProbeClearnet()}, "UNVERIFIED", "PROBE_NOT_CONFIRMED"},
		{"byo", deploymentProbeCase{routes: []sdkclient.Route{{Hostname: "byo.invalid", Upstream: "app:80"}}}, "UNVERIFIED", "PROBE_NOT_CONFIRMED"},
		{"slow-start", deploymentProbeCase{routes: deploymentProbeClearnet()}, "UNVERIFIED", "PROBE_NOT_CONFIRMED"},
		{"route-failed-even-if-probe-passed", deploymentProbeCase{routes: deploymentProbeClearnet(), failDocker: "exec", probeResult: true}, "REFUSED", "ROUTE_APPLY_FAILED"},
		{"no-probe", deploymentProbeCase{probeResult: true}, "NOT_CHECKED", "NO_PROBE"},
		{"onion", deploymentProbeCase{routes: []sdkclient.Route{{Upstream: "app:80", Onion: &sdkclient.RouteOnion{Enabled: true}}}, onion: onion, probeResult: true}, "NOT_APPLICABLE", "ONION_ONLY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deploymentProbeRun(t, tc.input)
			deploymentProbeRequireCaller(t, got)
			r := got.result.Readiness
			if got.result.Status != "success" || r == nil || r.Installation != "COMPLETED" || r.PublicEndpoint != tc.endpoint || r.Reason != tc.reason {
				t.Fatal("unexpected readiness")
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{deploymentProbeHost, "byo.invalid", onion, "app:80", "cmd_", "dpl_"} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatal("readiness leaked identity")
				}
			}
			if strings.Contains(got.docker, " down") || strings.Contains(got.docker, " stop ") {
				t.Fatal("destructive readiness effect")
			}
		})
	}
}

// Cancel DURING the single probe in the real caller, after route application.
// Cancellation is not a failed recipe or a claim of verified public readiness.
func TestDeploymentReadinessCallerCancellation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
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
	if err = os.WriteFile(filepath.Join(bin, name), raw, 0700); err != nil {
		t.Fatal(err)
	}
	dockerLog := filepath.Join(root, "docker.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(deploymentProbeFakeDockerEnv, "1")
	t.Setenv("DEPLOYMENT_PROBE_DOCKER_LOG", dockerLog)
	t.Setenv("DEPLOYMENT_PROBE_DOCKER_FAIL", "")
	state := t.TempDir()
	d := &Docker{StateDir: state, Log: discardLogger(), Proxy: proxy.New(state, discardLogger())}
	id := "dpl_" + strings.Repeat("e", 16)
	dir := d.appDir(id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	orig := probeHTTPS
	t.Cleanup(func() { probeHTTPS = orig })
	calls := 0
	probeHTTPS = func(context.Context, string, *slog.Logger) bool { calls++; cancel(); return false }
	p := sdkclient.DeployPayload{DeploymentID: id, Vars: map[string]any{"DEPLOYMENT_ID": id}, Routes: deploymentProbeClearnet(), Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", ComposeYAML: "services: {}"}}}
	result := d.finishReplacement(ctx, &sdkclient.PollCommand{ID: "cmd_readiness"}, p, nil, false, "")
	logged, _ := os.ReadFile(dockerLog)
	deploymentProbeRequireCaller(t, deploymentProbeOutcome{result: result, docker: string(logged)})
	if calls != 1 || result.Readiness == nil || result.Readiness.PublicEndpoint != "UNVERIFIED" || result.Readiness.Reason != "CANCELLED" {
		t.Fatal("cancellation promoted or retried")
	}
}

func TestDeploymentReadinessRunningIsNotUsable(t *testing.T) {
	for _, status := range []string{"running", "", "success"} {
		r := (deployReadinessObservation{}).result(status)
		if r.PublicEndpoint == "CONFIRMED" {
			t.Fatal("running/install success promoted to HTTPS")
		}
	}
}
