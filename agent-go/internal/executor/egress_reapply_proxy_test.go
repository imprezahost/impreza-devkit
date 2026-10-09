package executor

// The two halves of the serialized egress reapply.
//
// 1. The proxy-network reapply through the REAL deploy entry: a compose
//    that joins the shared impreza-proxy network (a Caddy wired, the
//    dockertest double answering network inspect/create) must reapplied
//    the egress at BOTH points — proxy network (before the stack) and
//    stack network (after compose up). The base (no reapply at the proxy
//    point) fails this test; removing only the stack point also fails.
//
// 2. The serialization lives in the egress package now: a test there
//    (egress package, apply_serial_test.go) proves concurrent Apply calls
//    cannot interleave their critical sections.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func proxyReapplyCompose() string {
	return "services:\n  app:\n    image: busybox:1.37.0\n    command: [\"sh\", \"-c\", \"exec sleep infinity\"]\n    restart: always\n    networks:\n      - default\n      - impreza-proxy\nnetworks:\n  impreza-proxy:\n    external: true\n"
}

func proxyReapplyCommand(id string) *sdkclient.PollCommand {
	payload := map[string]any{
		"deployment_id": id,
		"manifest": map[string]any{
			"name": "egress-reapply", "version": "1",
			"runtime": map[string]any{"type": "docker-compose", "compose_yaml": proxyReapplyCompose()},
		},
		"vars":   map[string]any{"DEPLOYMENT_ID": id, "HOST_PORT": "18048", "DOMAIN": "egress-reapply.invalid"},
		"routes": []map[string]any{{"hostname": "egress-reapply.invalid", "target_port": 8080, "upstream": "app:8080"}},
	}
	raw, _ := json.Marshal(payload)
	return &sdkclient.PollCommand{ID: "cmd_proxyre_" + id[len(id)-4:], Kind: sdkclient.CommandDeploy, Payload: json.RawMessage(raw)}
}

// TestEgressReapplyProxyNetworkReappliesThroughTheRealEntry drives Execute with a
// real proxy.Caddy wired (dockertest answers its network calls) and asserts
// the reapply order: proxy network BEFORE stack network.
func TestEgressReapplyProxyNetworkReappliesThroughTheRealEntry(t *testing.T) {
	id := "dpl_ee02aaaaaaaaaaa1"
	var calls []string
	d := &Docker{
		StateDir: t.TempDir(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Proxy:    &proxy.Caddy{StateDir: t.TempDir(), Log: testLoggerEgressReapply()},
		EgressApply: func(ctx context.Context, stateDir string) error {
			stage := ctx.Value(egressReapplyStage{}).(string)
			calls = append(calls, stage)
			return nil
		},
	}
	dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{
		dockertest.ComposeContainer(id, "app", "running", 0),
	}})
	result := d.Execute(context.Background(), proxyReapplyCommand(id))
	if result.Status != "success" {
		t.Fatalf("deploy failed: %s", result.Error)
	}
	// The proxy network may already exist on a warm host (EnsureNetwork is
	// idempotent); on a cold double it is created. Either way the deploy
	// must have reapplied at the proxy point and then at the stack point.
	joined := strings.Join(calls, "|")
	if joined != "proxy network|stack network" {
		t.Fatalf("expected proxy-network then stack-network reapplies through the real entry, got %q", joined)
	}
}

// TestEgressReapplyProxyNetworkReapplyFailsClosed: a reapply failure at the proxy
// point fails the deploy (the network never stays silently open).
func TestEgressReapplyProxyNetworkReapplyFailsClosed(t *testing.T) {
	id := "dpl_ee02bbbbbbbbbbb2"
	stateDir := t.TempDir()
	// Enforced (the fail-closed contract) reads egress.json: pre-record a v4
	// half that WAS in force, so the failed reapply fails closed instead
	// of taking the fail-open path of a host without the baseline.
	if err := os.WriteFile(filepath.Join(stateDir, "egress.json"),
		[]byte(`{"v4":{"applied":true,"last_attempt":"2026-10-07T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Docker{
		StateDir: stateDir,
		Log:      testLoggerEgressReapply(),
		Proxy:    &proxy.Caddy{StateDir: t.TempDir(), Log: testLoggerEgressReapply()},
		EgressApply: func(ctx context.Context, stateDir string) error {
			if ctx.Value(egressReapplyStage{}).(string) == "proxy network" {
				return errEgressReapplyFixture
			}
			return nil
		},
	}
	dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{
		dockertest.ComposeContainer(id, "app", "running", 0),
	}})
	result := d.Execute(context.Background(), proxyReapplyCommand(id))
	if result.Status == "success" || !strings.Contains(result.Error, "egress reapply failed") {
		t.Fatalf("a failed proxy-network reapply must fail the deploy, got: %+v", result)
	}
}

func testLoggerEgressReapply() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var errEgressReapplyFixture = &egressReapplyError{}

type egressReapplyError struct{}

func (*egressReapplyError) Error() string { return "egress-reapply fixture: iptables-restore failed" }
