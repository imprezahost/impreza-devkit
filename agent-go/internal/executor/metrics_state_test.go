package executor

// The `down` alert reads the state of each app's metrics point, and
// the collector folded a stack into "some container is running": with only
// the web of a two-service app stopped, the point said running and no alert
// opened. The state is running only when every service the stack expects
// to keep running has a running container; a declared one-shot that
// finished keeps counting as done. These go through CollectAppMetrics
// against the docker double and use only what the 0.6.23 agent had, so the
// same file also runs against that agent, where it fails.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const metricsApp = "dpl_0c8a000000000001"

func metricsFixture(t *testing.T, services []string, containers ...dockertest.Container) *Docker {
	t.Helper()
	return namedMetricsFixture(t, "", services, containers...)
}

// namedMetricsFixture writes a compose file with this top-level name ("" for none).
func namedMetricsFixture(t *testing.T, name string, services []string, containers ...dockertest.Container) *Docker {
	t.Helper()
	dockertest.Install(t, dockertest.State{Containers: containers})
	d := &Docker{StateDir: t.TempDir()}
	if err := os.MkdirAll(d.appDir(metricsApp), 0o700); err != nil {
		t.Fatal(err)
	}
	var compose strings.Builder
	if name != "" {
		compose.WriteString("name: " + name + "\n")
	}
	compose.WriteString("services:\n")
	for _, service := range services {
		compose.WriteString("  " + service + ":\n    image: busybox:1.36\n")
	}
	if err := os.WriteFile(filepath.Join(d.appDir(metricsApp), "compose.yaml"), []byte(compose.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func metricsPoint(t *testing.T, d *Docker) sdkclient.AppMetrics {
	t.Helper()
	for _, app := range d.CollectAppMetrics(context.Background()).Apps {
		if app.DeploymentID == metricsApp {
			return app
		}
	}
	t.Fatal("no metrics point for the app")
	return sdkclient.AppMetrics{}
}

func stackContainer(name, status string, exitCode int) dockertest.Container {
	return dockertest.ComposeContainer(metricsApp, name, status, exitCode)
}

func TestMetricsStateSeesAStoppedServiceBesideARunningOne(t *testing.T) {
	// `docker stop` on the web: exit 0, the database still up.
	d := metricsFixture(t, []string{"web", "db"}, stackContainer("web", "exited", 0), stackContainer("db", "running", 0))
	if got := metricsPoint(t, d).State; got == "running" {
		t.Fatal("a stopped web beside a running database still reads running: the down alert never opens")
	}
}

func TestMetricsStateSeesADeclaredServiceWithoutAContainer(t *testing.T) {
	d := metricsFixture(t, []string{"web", "db"}, stackContainer("db", "running", 0))
	if got := metricsPoint(t, d).State; got == "running" {
		t.Fatal("a declared service with no container at all still reads running")
	}
}

// With no container at all the point read "unknown", a state the server
// discards, so the down alert had no point to open on either.
func TestMetricsStateSeesAnAppWithNoContainers(t *testing.T) {
	d := metricsFixture(t, []string{"web", "db"})
	if got := metricsPoint(t, d).State; got != "exited" {
		t.Fatalf("an app with no container reads %q, which the server drops; want exited", got)
	}
}

func TestMetricsStateSeesAFailedInitJob(t *testing.T) {
	initJob := stackContainer("init", "exited", 1)
	app := stackContainer("web", "running", 0)
	app.Config.Labels["com.docker.compose.depends_on"] = "init:service_completed_successfully:false"
	d := metricsFixture(t, []string{"init", "web"}, initJob, app)
	if got := metricsPoint(t, d).State; got == "running" {
		t.Fatal("a failed one-shot the app depends on still reads running")
	}
}

// A compose file that names its project literally runs under that name,
// not the deployment ID: its containers are the app's. Reading only the
// ID's project found none (the point was unknown); with the per-service verdict
// that would report a running app as exited and open the down alert.
func TestMetricsFollowTheAppsComposeProject(t *testing.T) {
	web := dockertest.ComposeContainer("shop-legacy", "web", "running", 0)
	db := dockertest.ComposeContainer("shop-legacy", "db", "running", 0)
	d := namedMetricsFixture(t, "shop-legacy", []string{"web", "db"}, web, db)
	if got := metricsPoint(t, d).State; got != "running" {
		t.Fatalf("a running stack with a literal project name reads %q", got)
	}
}

// Guards: these read the same before and after the fix.

func TestMetricsStateCountsACompletedOneShotAsDone(t *testing.T) {
	initJob := stackContainer("init", "exited", 0)
	app := stackContainer("web", "running", 0)
	app.Config.Labels["com.docker.compose.depends_on"] = "init:service_completed_successfully:false"
	d := metricsFixture(t, []string{"init", "web"}, initJob, app)
	if got := metricsPoint(t, d).State; got != "running" {
		t.Fatalf("a finished, declared one-shot must not take the app down: %q", got)
	}
}

func TestMetricsStateWholeStack(t *testing.T) {
	cases := []struct {
		name       string
		containers []dockertest.Container
		want       string
	}{
		{"all running", []dockertest.Container{stackContainer("web", "running", 0), stackContainer("db", "running", 0)}, "running"},
		{"restarting outranks", []dockertest.Container{stackContainer("web", "restarting", 0), stackContainer("db", "running", 0)}, "restarting"},
		{"all stopped", []dockertest.Container{stackContainer("web", "exited", 0), stackContainer("db", "exited", 137)}, "exited"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := metricsFixture(t, []string{"web", "db"}, c.containers...)
			if got := metricsPoint(t, d).State; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A partly stopped app keeps reporting what its running containers use, so
// the memory and CPU alerts still see it.
func TestMetricsOfAPartlyStoppedAppKeepUsage(t *testing.T) {
	d := metricsFixture(t, []string{"web", "db"}, stackContainer("web", "exited", 0), stackContainer("db", "running", 0))
	if point := metricsPoint(t, d); point.MemoryBytes == 0 || point.CPUPercent == 0 {
		t.Fatalf("the running database's usage was dropped: cpu %.2f, memory %d", point.CPUPercent, point.MemoryBytes)
	}
}
