package executor

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestOnionPublicationAdvisoryAndPrivacy(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		var logs bytes.Buffer
		d := &Docker{Log: slog.New(slog.NewTextHandler(&logs, nil))}
		onion := strings.Repeat("a", 56) + ".onion"
		calls := 0
		d.onionPublicationWait = func(_ context.Context, id, addr string) bool {
			calls++
			if id != "dpl_test" || addr != onion {
				t.Fatal("lost owned target")
			}
			return confirmed
		}
		note := d.onionPublicationNote(context.Background(), &sdkclient.PollCommand{}, "dpl_test", onion)
		if calls != 1 || strings.Contains(note, onion) || strings.Contains(logs.String(), onion) {
			t.Fatal("privacy or duplicate observer")
		}
		if !strings.Contains(logs.String(), "publishing .onion") {
			t.Fatal("publication phase not reported")
		}
		if confirmed && !strings.Contains(note, "daemon event") {
			t.Fatal("confirmation missing")
		}
		if !confirmed && (!strings.Contains(note, "publishing .onion") || !strings.Contains(note, "advisory")) {
			t.Fatal("unconfirmed service presented ready")
		}
	}
}

func TestOnionPublicationAbsentOrCancelledDoesNotConfirm(t *testing.T) {
	d := &Docker{}
	calls := 0
	d.onionPublicationWait = func(context.Context, string, string) bool { calls++; return true }
	if d.onionPublicationNote(context.Background(), &sdkclient.PollCommand{}, "id", "") != "" || calls != 0 {
		t.Fatal("clearnet deploy probed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	note := d.onionPublicationNote(ctx, &sdkclient.PollCommand{}, "id", "owned")
	if !strings.Contains(note, "advisory") || strings.Contains(note, "daemon event") {
		t.Fatal("cancelled observation confirmed")
	}
}

func TestOnionPublicationCallerCallerDoesNotFinishBeforeObservation(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	o := deploymentProbeRun(t, deploymentProbeCase{routes: deploymentProbeClearnet(), onion: onion, probeResult: true})
	deploymentProbeRequireCaller(t, o)
	if o.result.Status != "success" || o.result.Onion != onion {
		t.Fatal("deploy success or owner address lost")
	}
	if !strings.Contains(o.result.LogsTail, "publishing .onion") || strings.Contains(o.result.LogsTail, onion) {
		t.Fatal("finished before observation or wrote the address into the tail")
	}
	// ApplyDeploymentRoutes reloads Caddy once, before onionPublicationNote.
	// That reload is the route apply, not a second replacement and not the
	// onion probe. A down or another compose up still means the caller
	// repeated or tore the work down.
	reload := "exec " + proxy.ContainerName + " caddy reload"
	if strings.Count(o.docker, " up -d ") != 1 || strings.Contains(o.docker, " down") || strings.Count(o.docker, reload) != 1 {
		t.Fatalf("replacement repeated or tore down work:\n%s", o.docker)
	}
}

func TestOnionPublicationCallerCallerConfirmsOnlyAfterCompose(t *testing.T) {
	onion := strings.Repeat("c", 56) + ".onion"
	seenUp := false
	o := deploymentProbeRun(t, deploymentProbeCase{
		routes:      deploymentProbeClearnet(),
		onion:       onion,
		probeResult: true,
		onionWait: func(_ context.Context, id, addr string) bool {
			if id == "" || addr != onion {
				t.Fatal("caller lost the owned target")
			}
			raw, err := os.ReadFile(os.Getenv("DEPLOYMENT_PROBE_DOCKER_LOG"))
			if err != nil {
				t.Fatal(err)
			}
			seenUp = strings.Contains(string(raw), " up -d ")
			return true
		},
	})
	deploymentProbeRequireCaller(t, o)
	if !seenUp || o.result.Status != "success" || !strings.Contains(o.result.LogsTail, "daemon event") {
		t.Fatal("confirmation was not after the real replacement")
	}
	if strings.Contains(o.result.LogsTail, onion) || strings.Count(o.docker, " up -d ") != 1 {
		t.Fatal("address leaked or compose repeated")
	}
}

// This uses the real finishReplacement -> Tor.WaitForOnionPublication path,
// without substituting the publication observer. An old/unconfigured image has
// no controller socket and must NOT spend the former six-minute curl budget.
// Measure the caller itself, excluding fixture setup and fake-executable warm-up.
func TestOnionPublicationCallerCallerMissingControllerDoesNotWaitForImageTool(t *testing.T) {
	onion := strings.Repeat("f", 56) + ".onion"
	o := deploymentProbeRun(t, deploymentProbeCase{routes: deploymentProbeClearnet(), onion: onion, probeResult: true, realTor: true, timeout: 5 * time.Second})
	deploymentProbeRequireCaller(t, o)
	if o.caller >= 4*time.Second {
		t.Fatalf("missing controller/tool delayed the real caller (%s)", o.caller)
	}
	if o.result.Status != "success" || !strings.Contains(o.result.LogsTail, "descriptor upload") || !strings.Contains(o.result.LogsTail, "advisory") {
		t.Fatal("missing controller is not an honest descriptor advisory")
	}
	if strings.Contains(o.docker, "curl") || strings.Contains(o.docker, onion) || strings.Count(o.docker, "exec "+proxy.ContainerName+" caddy reload") != 1 {
		t.Fatal("external client, address disclosure or repeated route reload")
	}
}

// The measured caller time still includes the caller's own waiting: only the
// fixture setup and the fake-executable warm-up are excluded.
func TestOnionPublicationCallerTimeIncludesItsOwnWait(t *testing.T) {
	onion := strings.Repeat("e", 56) + ".onion"
	const wait = 300 * time.Millisecond
	o := deploymentProbeRun(t, deploymentProbeCase{
		routes:      deploymentProbeClearnet(),
		onion:       onion,
		probeResult: true,
		onionWait: func(context.Context, string, string) bool {
			time.Sleep(wait)
			return true
		},
	})
	deploymentProbeRequireCaller(t, o)
	if o.caller < wait {
		t.Fatalf("caller time %s excludes the caller's own %s wait", o.caller, wait)
	}
}
