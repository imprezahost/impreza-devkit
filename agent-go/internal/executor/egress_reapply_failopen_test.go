package executor

// The egress reapply fails closed only where the egress baseline
// was in force. On a host where it never applies (Docker "iptables": false: no
// DOCKER-USER chain; no iptables at all) the agent keeps its fail-open
// contract, so deploys still work there. Failing closed everywhere failed every
// deploy on such a host (measured on a real one), and the restore-quiesce job
// tests that run without an egress baseline failed for the same reason.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
)

func egressEnforcedStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "egress.json"), []byte(`{"v4":{"applied":true,"last_attempt":"","rules":1,"resolvers":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHostWithoutEgressBaselineKeepsDeploying(t *testing.T) {
	calls := 0
	d := &egressReapplyFixture{Docker: &Docker{
		StateDir: t.TempDir(), // no egress.json: the baseline never applied here
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		EgressApply: func(ctx context.Context, stateDir string) error {
			calls++
			return errors.New("egress parent chain unavailable; docker may not be running yet")
		},
	}}
	dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{
		dockertest.ComposeContainer("dpl_ee01fffffffffff9", "app", "running", 0),
	}})
	res := d.Execute(context.Background(), egressReapplyCommand("dpl_ee01fffffffffff9", false))
	if res.Status != "success" {
		t.Fatalf("a host without the egress baseline must keep deploying (fail-open contract), got %s: %s", res.Status, res.Error)
	}
	if calls != 1 {
		t.Fatalf("the reapply must still be attempted once, got %d", calls)
	}
}
