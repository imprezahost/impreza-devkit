package executor

// A deployment whose preparation died before compose.yaml existed
// has nothing for `docker compose down` (or `logs`) to act on. The
// uninstall is a clean no-op success (label sweep, rest cleanup, no
// misleading error tail), the rollback of a failed first deploy sweeps
// directly, and the logs answer says there are no containers — instead
// of the compose failure "no configuration file provided: not found".

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
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func noComposeFixture(t *testing.T, withCompose bool) (*Docker, string, string) {
	t.Helper()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := "dpl_c0de500000000c0d"
	dockertest.Install(t, dockertest.State{})
	dir := d.appDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withCompose {
		if err := os.WriteFile(filepath.Join(dir, composeFileName), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := json.Marshal(sdkclient.UninstallPayload{DeploymentID: id, PurgeData: false})
	return d, id, string(payload)
}

// The uninstall of a directory that never had compose.yaml succeeds
// WITHOUT the compose error tail — on the pre-fix code the tail says
// "compose down reported an error ... no configuration file provided".
func TestUninstallWithoutComposeFileIsCleanNoOp(t *testing.T) {
	d, id, payload := noComposeFixture(t, false)
	cmd := &sdkclient.PollCommand{ID: "cmd_nocompose", Kind: sdkclient.CommandUninstall, Payload: []byte(payload)}
	result := d.uninstall(context.Background(), cmd)
	if result.Status != "success" {
		t.Fatalf("uninstall of a never-deployed app failed: %+v", result)
	}
	if strings.Contains(result.LogsTail, "compose down reported an error") ||
		strings.Contains(result.LogsTail, "no configuration file") {
		t.Fatalf("the clean no-op still carries the compose error tail: %q", result.LogsTail)
	}
	// The transient artifacts are gone; data/ is kept (purge_data=false).
	if _, err := os.Stat(filepath.Join(d.appDir(id), "compose.yaml")); !os.IsNotExist(err) {
		t.Fatal("a compose.yaml appeared during the uninstall")
	}
	if got, err := os.ReadFile(filepath.Join(d.appDir(id), "data", "keep.txt")); err != nil || string(got) != "x" {
		t.Fatal("data/ was not retained")
	}
}

// The same shape with compose.yaml present still goes through compose
// down (the guard does not swallow the real path).
func TestUninstallWithComposeFileStillRunsDown(t *testing.T) {
	d, _, payload := noComposeFixture(t, true)
	result := d.uninstall(context.Background(), &sdkclient.PollCommand{ID: "cmd_nocompose", Kind: sdkclient.CommandUninstall, Payload: []byte(payload)})
	if result.Status != "success" {
		t.Fatalf("uninstall failed: %+v", result)
	}
}

// The logs of a deployment that never had compose.yaml answer with a
// final chunk and success — not with the compose failure.
func TestLogsWithoutComposeFileAnswersNoContainers(t *testing.T) {
	d, id, _ := noComposeFixture(t, false)
	payload, _ := json.Marshal(sdkclient.LogsTailPayload{DeploymentID: id, StreamID: "st_nocompose"})
	cmd := &sdkclient.PollCommand{ID: "cmd_nocompose_logs", Kind: sdkclient.CommandLogsTail, Payload: payload}
	result := d.logsTail(context.Background(), cmd)
	if result.Status != "success" {
		t.Fatalf("logs of a never-deployed app failed: %+v", result)
	}
}

// The rollback of a failed first deploy (no compose.yaml) sweeps by
// label and does not report a compose failure.
func TestRollbackWithoutComposeFileSweeps(t *testing.T) {
	d, id, _ := noComposeFixture(t, false)
	d.rollbackFailedDeploy(context.Background(), d.appDir(id), id, false, "clone failed")
	if _, err := os.Stat(filepath.Join(d.appDir(id), "data", "keep.txt")); err != nil {
		t.Fatal("the no-compose rollback touched data/ (it must only sweep containers)")
	}
}
