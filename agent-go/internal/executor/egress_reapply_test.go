package executor

// A network the executor creates after the last egress apply (the
// shared proxy network, the deployment's own compose network at stack up)
// carries no per-interface rules until the periodic reconcile. The executor
// now reapplies the egress right after each creation point and FAILS
// CLOSED. These tests drive the REAL Execute entry against dockertest and
// observe the seam; on the previous release there is no seam and no reapply
// (the open window was also measured live on a real host).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func egressReapplyCompose(joinProxy bool) string {
	networks := ""
	if joinProxy {
		networks = "\n    networks:\n      - default\n      - impreza-proxy\n"
	}
	return "services:\n  app:\n    image: busybox:1.37.0\n    command: [\"sh\", \"-c\", \"exec sleep infinity\"]\n    restart: always" + networks + "\nnetworks:\n  impreza-proxy:\n    external: true\n"
}

func egressReapplyCommand(id string, joinProxy bool) *sdkclient.PollCommand {
	payload := map[string]any{
		"deployment_id": id,
		"manifest": map[string]any{
			"name": "egress-reapply", "version": "1",
			"runtime": map[string]any{
				"type": "docker-compose", "compose_yaml": egressReapplyCompose(joinProxy),
			},
		},
		"vars": map[string]any{"DEPLOYMENT_ID": id, "HOST_PORT": "18047", "DOMAIN": "egress-reapply.invalid"},
	}
	if joinProxy {
		payload["routes"] = []map[string]any{{
			"hostname": "egress-reapply.invalid", "target_port": 8080, "upstream": "app:8080",
		}}
	}
	raw, _ := json.Marshal(payload)
	return &sdkclient.PollCommand{
		ID: "cmd_egress_" + id[len(id)-4:], Kind: sdkclient.CommandDeploy,
		Payload: json.RawMessage(raw),
	}
}

type egressReapplyFixture struct {
	*Docker
	calls    []string
	egressFn func(ctx context.Context, stateDir string) error
}

func egressReapplyDeploy(t *testing.T, id string, joinProxy bool) (*egressReapplyFixture, sdkclient.DeployResult) {
	t.Helper()
	f := &egressReapplyFixture{}
	f.Docker = &Docker{
		StateDir: t.TempDir(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EgressApply: func(ctx context.Context, stateDir string) error {
			stage := ctx.Value(egressReapplyStage{}).(string)
			f.calls = append(f.calls, stage)
			if f.egressFn != nil {
				return f.egressFn(ctx, stateDir)
			}
			return nil
		},
	}
	dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{
		dockertest.ComposeContainer(id, "app", "running", 0),
	}})
	return f, f.Execute(context.Background(), egressReapplyCommand(id, joinProxy))
}

type egressReapplyStageKey struct{}

// TestStackUpReappliesEgress: the compose up creates the deployment's
// bridge — exactly one reapply ("stack network") before success.
func TestStackUpReappliesEgress(t *testing.T) {
	f, result := egressReapplyDeploy(t, "dpl_ee01aaaaaaaaaaa1", false)
	if result.Status != "success" {
		t.Fatalf("deploy failed: %s", result.Error)
	}
	if strings.Join(f.calls, "|") != "stack network" {
		t.Fatalf("expected one stack-network reapply, got %v", f.calls)
	}
}

// TestProxyJoinStillReappliesEgressOnce: a compose joining the shared proxy
// network (Proxy not wired in this fixture) goes through the same
// stack-up path — one reapply, success. The proxy-network reapply point
// itself is covered by the Linux host tests with the real Caddy.
func TestProxyJoinStillReappliesEgressOnce(t *testing.T) {
	f, result := egressReapplyDeploy(t, "dpl_ee01bbbbbbbbbbb2", true)
	if result.Status != "success" {
		t.Fatalf("deploy failed: %s", result.Error)
	}
	if strings.Join(f.calls, "|") != "stack network" {
		t.Fatalf("expected one stack-network reapply, got %v", f.calls)
	}
}

// TestEgressReapplyFailsClosedWhenItFails: a failed reapply fails the
// deploy — the new bridge never stays open in silence.
func TestEgressReapplyFailsClosedWhenItFails(t *testing.T) {
	// The egress baseline is in force on this host (fail closed only there).
	dir := egressEnforcedStateDir(t)
	f2 := &egressReapplyFixture{Docker: &Docker{
		StateDir: dir,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EgressApply: func(ctx context.Context, stateDir string) error {
			return errors.New("iptables-restore: line 3 failed")
		},
	}}
	dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{
		dockertest.ComposeContainer("dpl_ee01ccccccccccc3", "app", "running", 0),
	}})
	res := f2.Execute(context.Background(), egressReapplyCommand("dpl_ee01ccccccccccc3", false))
	if res.Status == "success" {
		t.Fatal("a failed egress reapply must fail the deploy (fail closed)")
	}
	if !strings.Contains(res.Error, "egress reapply failed") {
		t.Fatalf("the failure must name the egress reapply, got: %s", res.Error)
	}
}
