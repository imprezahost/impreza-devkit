package executor

import (
	"context"
	"encoding/json"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestUninstallRefusesSurvivingContainer(t *testing.T) {
	for _, hasState := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-state", true: "failed-down"}[hasState], func(t *testing.T) {
			id := "dpl_uninstall_test"
			d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			container := dockertest.ComposeContainer(id, "app", "running", 0)
			other := dockertest.ComposeContainer("dpl_other_owner", "app", "running", 0)
			if hasState {
				dir := d.appDir(id)
				os.MkdirAll(dir, 0700)
				os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("name: "+id+"\nservices:\n  app:\n    image: app:1\n"), 0600)
				os.WriteFile(filepath.Join(dir, ".env"), []byte("RECOVERY=preserve\n"), 0600)
			}
			double := dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{container, other}, DownSurvivors: []string{container.ID}, Unremovable: []string{container.ID}})
			payload, _ := json.Marshal(sdkclient.UninstallPayload{DeploymentID: id, PurgeData: true})
			result := d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_uninstall", Kind: sdkclient.CommandUninstall, Payload: payload})
			if result.Status != "failed" {
				t.Fatalf("uninstall claimed success with a running container: %s", result.Status)
			}
			state := double.State()
			if len(state.Containers) != 2 {
				t.Fatal("failed uninstall changed other owner or hid surviving app")
			}
			if hasState {
				if _, err := os.Stat(filepath.Join(d.appDir(id), ".env")); err != nil {
					t.Fatal("failed uninstall discarded retry configuration")
				}
			}
		})
	}
}

// An app deployed before its project was derived from the deployment id
// carries the literal name of its compose.yaml. Its containers must be
// cleaned under that project too: removing only the derived project left
// them running behind a successful uninstall. Runs on the Docker double,
// so the guard holds on any platform.
func TestUninstallCleansLegacyComposeProject(t *testing.T) {
	const id, legacy = "dpl_legacy_app", "legacy_named_app"
	setup := func(t *testing.T, unremovable bool) (*Docker, *dockertest.Double, dockertest.Container) {
		t.Helper()
		d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		dir := d.appDir(id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("name: "+legacy+"\nservices:\n  app:\n    image: app:1\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("RECOVERY=preserve\n"), 0600); err != nil {
			t.Fatal(err)
		}
		app := dockertest.ComposeContainer(legacy, "app", "running", 0)
		other := dockertest.ComposeContainer("dpl_other_owner", "app", "running", 0)
		state := dockertest.State{Containers: []dockertest.Container{app, other}, DownSurvivors: []string{app.ID}}
		if unremovable {
			state.Unremovable = []string{app.ID}
		}
		return d, dockertest.Install(t, state), app
	}
	uninstall := func(d *Docker) sdkclient.DeployResult {
		payload, _ := json.Marshal(sdkclient.UninstallPayload{DeploymentID: id, PurgeData: true})
		return d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_legacy_uninstall", Kind: sdkclient.CommandUninstall, Payload: payload})
	}
	t.Run("removed-under-the-literal-name", func(t *testing.T) {
		d, double, app := setup(t, false)
		if result := uninstall(d); result.Status != "success" {
			t.Fatalf("the legacy app could not be uninstalled: %s %s", result.Status, result.Error)
		}
		for _, c := range double.State().Containers {
			if c.ID == app.ID {
				t.Fatal("successful uninstall left the legacy project's container running")
			}
		}
		if _, ok := double.Container("dpl_other_owner-app-1"); !ok {
			t.Fatal("uninstall touched another owner")
		}
	})
	t.Run("survivor-under-the-literal-name", func(t *testing.T) {
		d, double, _ := setup(t, true)
		if result := uninstall(d); result.Status != "failed" {
			t.Fatalf("uninstall claimed success with the legacy project's container alive: %s", result.Status)
		}
		if len(double.State().Containers) != 2 {
			t.Fatal("failed uninstall changed another owner or hid the surviving legacy container")
		}
		if _, err := os.Stat(filepath.Join(d.appDir(id), ".env")); err != nil {
			t.Fatal("failed uninstall discarded retry configuration")
		}
	})
}
