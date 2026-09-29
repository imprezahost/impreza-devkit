package executor

// The agent ran Compose in the app directory with neither -p nor -f,
// and Compose reads COMPOSE_PROJECT_NAME and COMPOSE_FILE from the .env:
// one variable pointed an app's Compose at another app of the same host,
// whose containers and volumes its uninstall (`down --volumes`) then
// removed. The docker double resolves the project with Compose's own
// precedence and `compose down` removes that project's containers, so these
// tests see the other app's containers disappear, not just a missing flag.
// They go through Execute and CollectRuntime and use only what the 0.6.23
// agent had, so the same file also runs against that agent, where it fails.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const (
	x71App   = "dpl_0c1a000000000001"
	x71Other = "dpl_0c1b000000000002"
)

type x71Host struct {
	d      *Docker
	double *dockertest.Double
}

// x71Fixture is a host with two apps; files maps an app to its compose.yaml
// and .env contents (a missing entry keeps a plain compose and no .env).
func x71Fixture(t *testing.T, files map[string][2]string, containers ...dockertest.Container) x71Host {
	t.Helper()
	double := dockertest.Install(t, dockertest.State{Containers: containers})
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, id := range []string{x71App, x71Other} {
		dir := d.appDir(id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		compose, env := "services:\n  web:\n    image: busybox:1.36\n", ""
		if f, ok := files[id]; ok {
			compose, env = f[0], f[1]
		}
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
		if env != "" {
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return x71Host{d: d, double: double}
}

func (h x71Host) execute(t *testing.T, kind sdkclient.CommandKind, payload any) sdkclient.DeployResult {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return h.d.Execute(ctx, &sdkclient.PollCommand{ID: "cmd_project" + string(kind), Kind: kind, Payload: raw})
}

func (h x71Host) running(project string) int {
	n := 0
	for _, c := range h.double.State().Containers {
		if c.Config.Labels["com.docker.compose.project"] == project {
			n++
		}
	}
	return n
}

func x71Containers() []dockertest.Container {
	return []dockertest.Container{
		dockertest.ComposeContainer(x71App, "web", "running", 0),
		dockertest.ComposeContainer(x71Other, "web", "running", 0),
		dockertest.ComposeContainer(x71Other, "db", "running", 0),
	}
}

func TestUninstallNeverActsOnAnotherAppsProject(t *testing.T) {
	cases := map[string]func(h x71Host) string{
		// What an older agent wrote for an app variable of that name.
		"COMPOSE_PROJECT_NAME": func(x71Host) string { return "COMPOSE_PROJECT_NAME='" + x71Other + "'\n" },
		"COMPOSE_FILE": func(h x71Host) string {
			return "COMPOSE_FILE='" + filepath.Join(h.d.appDir(x71Other), "compose.yaml") + "'\n"
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			h := x71Fixture(t, nil, x71Containers()...)
			if err := os.WriteFile(filepath.Join(h.d.appDir(x71App), ".env"), []byte(env(h)), 0o600); err != nil {
				t.Fatal(err)
			}
			res := h.execute(t, sdkclient.CommandUninstall, sdkclient.UninstallPayload{DeploymentID: x71App, PurgeData: true})
			if res.Status != "success" {
				t.Fatalf("uninstall failed: %s", res.Error)
			}
			if got := h.running(x71Other); got != 2 {
				t.Fatalf("the uninstall of one app removed the other app's containers through %s (%d of 2 left)", name, got)
			}
			if got := h.running(x71App); got != 0 {
				t.Fatalf("the uninstalled app kept %d container(s)", got)
			}
		})
	}
}

// A top-level name built from a variable is the same redirection through
// the compose file. It is never followed; the uninstall still removes the
// app's own containers through the label sweep.
func TestUninstallNeverFollowsAProjectNameVariable(t *testing.T) {
	h := x71Fixture(t, map[string][2]string{
		x71App: {"name: ${APP_NAME}\nservices:\n  web:\n    image: busybox:1.36\n", "APP_NAME='" + x71Other + "'\n"},
	}, x71Containers()...)
	res := h.execute(t, sdkclient.CommandUninstall, sdkclient.UninstallPayload{DeploymentID: x71App, PurgeData: true})
	if res.Status != "success" {
		t.Fatalf("uninstall failed: %s", res.Error)
	}
	if got := h.running(x71Other); got != 2 {
		t.Fatalf("a project name taken from a variable removed the other app's containers (%d of 2 left)", got)
	}
	if got := h.running(x71App); got != 0 {
		t.Fatalf("the uninstalled app kept %d container(s)", got)
	}
}

// Guard: an app whose compose file names its own project literally keeps
// it, with its containers and named volumes, exactly as before the fix.
func TestUninstallKeepsALiteralProjectName(t *testing.T) {
	legacy := dockertest.ComposeContainer("legacy-shop", "web", "running", 0)
	h := x71Fixture(t, map[string][2]string{
		x71App: {"name: legacy-shop\nservices:\n  web:\n    image: busybox:1.36\n", ""},
	}, append(x71Containers(), legacy)...)
	res := h.execute(t, sdkclient.CommandUninstall, sdkclient.UninstallPayload{DeploymentID: x71App})
	if res.Status != "success" {
		t.Fatalf("uninstall failed: %s", res.Error)
	}
	if got := h.running("legacy-shop"); got != 0 {
		t.Fatalf("the app's literally named project kept %d container(s)", got)
	}
	if got := h.running(x71Other); got != 2 {
		t.Fatalf("another app lost containers (%d of 2 left)", got)
	}
}

func TestComposeRunsOnThePinnedProjectAndFile(t *testing.T) {
	h := x71Fixture(t, map[string][2]string{
		x71App: {"services:\n  web:\n    image: busybox:1.36\n", "COMPOSE_PROJECT_NAME='" + x71Other + "'\n"},
	}, x71Containers()...)
	for _, kind := range []sdkclient.CommandKind{sdkclient.CommandRestart, sdkclient.CommandHealthCheck} {
		if res := h.execute(t, kind, map[string]string{"deployment_id": x71App}); res.Status != "success" {
			t.Fatalf("%s failed: %s", kind, res.Error)
		}
	}
	h.d.CollectRuntime(context.Background())
	want := []string{"compose", "-p", x71App, "-f", filepath.Join(h.d.appDir(x71App), "compose.yaml")}
	calls := 0
	for _, call := range h.double.Calls("compose") {
		if call.Dir != h.d.appDir(x71App) {
			continue
		}
		calls++
		if len(call.Args) < len(want) || strings.Join(call.Args[:len(want)], " ") != strings.Join(want, " ") {
			t.Fatalf("Compose ran without the pinned project and file: %q", call.Args)
		}
	}
	if calls < 3 {
		t.Fatalf("expected restart, health check and the runtime collector to run Compose, got %d calls", calls)
	}
}

func TestDeployRefusesEngineAndMalformedVariableNames(t *testing.T) {
	for _, key := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_ENV_FILES", "DOCKER_HOST", "docker_host",
		"BAD\nKEY", "INJECTED='x'\nCOMPOSE_PROJECT_NAME", "KEY=VALUE", "1LEADING_DIGIT", "HAS SPACE", ""} {
		t.Run(strings.NewReplacer("\n", `\n`).Replace(key), func(t *testing.T) {
			h := x71Fixture(t, nil, x71Containers()...)
			appDir := h.d.appDir(x71App)
			if err := os.Remove(filepath.Join(appDir, "compose.yaml")); err != nil {
				t.Fatal(err)
			}
			res := h.execute(t, sdkclient.CommandDeploy, map[string]any{
				"deployment_id": x71App,
				"manifest":      map[string]any{"name": "compose-app", "version": "1", "runtime": map[string]any{"type": "docker-compose", "compose_yaml": "services:\n  web:\n    image: busybox:1.36\n"}},
				"vars":          map[string]any{"DEPLOYMENT_ID": x71App, key: x71Other},
			})
			if res.Status == "success" {
				t.Fatal("a deploy carrying an engine or malformed variable name was accepted")
			}
			for _, name := range []string{".env", "compose.yaml"} {
				if _, err := os.Stat(filepath.Join(appDir, name)); err == nil {
					t.Fatalf("the refused deploy still wrote %s", name)
				}
			}
			if calls := h.double.Calls("compose"); len(calls) > 0 {
				t.Fatalf("the refused deploy still ran Compose: %q", calls[0].Args)
			}
			if strings.ContainsAny(res.Error, "\n\r") || (key != "" && !envVariableNameShape(key) && strings.Contains(res.Error, key)) {
				t.Fatalf("the refusal echoes a malformed name: %q", res.Error)
			}
			if h.running(x71Other) != 2 {
				t.Fatal("the refused deploy touched another app")
			}
		})
	}
}

func TestRouteUpdateRefusesEngineVariableNames(t *testing.T) {
	h := x71Fixture(t, map[string][2]string{x71App: {"services:\n  web:\n    image: busybox:1.36\n", "APP='kept'\n"}}, x71Containers()...)
	res := h.execute(t, sdkclient.CommandUpdateRoutes, map[string]any{
		"deployment_id": x71App,
		"vars":          map[string]any{"APP": "changed", "COMPOSE_PROJECT_NAME": x71Other},
	})
	if res.Status == "success" {
		t.Fatal("a route update carrying COMPOSE_PROJECT_NAME was accepted")
	}
	if env, _ := os.ReadFile(filepath.Join(h.d.appDir(x71App), ".env")); string(env) != "APP='kept'\n" {
		t.Fatalf("the refused route update rewrote the .env: %q", env)
	}
}

// envVariableNameShape is the plain-name grammar, restated so this file
// compiles against the base agent.
func envVariableNameShape(key string) bool {
	if key == "" || (key[0] >= '0' && key[0] <= '9') {
		return false
	}
	for _, r := range key {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
