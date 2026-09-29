package executor

// Two numbers of the per-app metrics misled. memory_limit_bytes added
// each container's limit as docker stats prints it, and a container without
// one prints the host's memory, so two such containers on an 8 GB server
// reported 15.5 GiB. volume_bytes counted only named volumes whose name
// started with the deployment ID, so the ./data bind mount most catalog
// apps use read 0, and so did the importer's ${DEPLOYMENT_ID}-vol-* volumes.
// These go through CollectAppMetrics against the docker double and use only
// what the 0.6.23 agent had, so the same file also runs against that agent,
// where it fails.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const numbersApp = "dpl_0c6a000000000001"

func numbersFixture(t *testing.T, state dockertest.State) *Docker {
	t.Helper()
	d := numbersApps(t)
	dockertest.Install(t, state)
	return d
}

// numbersApps writes the app's directory; the caller installs the double.
func numbersApps(t *testing.T) *Docker {
	t.Helper()
	d := &Docker{StateDir: t.TempDir()}
	if err := os.MkdirAll(d.appDir(numbersApp), 0o700); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: busybox:1.36\n  db:\n    image: busybox:1.36\n"
	if err := os.WriteFile(filepath.Join(d.appDir(numbersApp), "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func numbersPoint(t *testing.T, d *Docker) sdkclient.AppMetrics {
	t.Helper()
	for _, app := range d.CollectAppMetrics(context.Background()).Apps {
		if app.DeploymentID == numbersApp {
			return app
		}
	}
	t.Fatal("no metrics point for the app")
	return sdkclient.AppMetrics{}
}

func numbersContainer(service string, limit int64) dockertest.Container {
	c := dockertest.ComposeContainer(numbersApp, service, "running", 0)
	c.HostConfig.Memory = limit
	return c
}

func containerName(c dockertest.Container) string { return strings.TrimPrefix(c.Name, "/") }

const (
	mib        = int64(1) << 20
	hostMemory = int64(7.75 * float64(int64(1)<<30))
)

func TestMetricsMemoryLimitCountsTheHostOnce(t *testing.T) {
	web, db := numbersContainer("web", 0), numbersContainer("db", 0)
	d := numbersFixture(t, dockertest.State{Containers: []dockertest.Container{web, db}, Stats: map[string]dockertest.Stats{
		containerName(web): {CPUPerc: "1.00%", MemUsage: "100MiB / 7.75GiB"},
		containerName(db):  {CPUPerc: "1.00%", MemUsage: "200MiB / 7.75GiB"},
	}})
	point := numbersPoint(t, d)
	if point.MemoryBytes != 300*mib {
		t.Fatalf("memory in use %d, want %d", point.MemoryBytes, 300*mib)
	}
	if point.MemoryLimitBytes != hostMemory {
		t.Fatalf("two containers without a limit report %d bytes of limit, want the host's %d once", point.MemoryLimitBytes, hostMemory)
	}
}

func TestMetricsMemoryLimitWithOneUnlimitedContainer(t *testing.T) {
	web, db := numbersContainer("web", 0), numbersContainer("db", 512*mib)
	d := numbersFixture(t, dockertest.State{Containers: []dockertest.Container{web, db}, Stats: map[string]dockertest.Stats{
		containerName(web): {CPUPerc: "1.00%", MemUsage: "100MiB / 7.75GiB"},
		containerName(db):  {CPUPerc: "1.00%", MemUsage: "200MiB / 512MiB"},
	}})
	if got := numbersPoint(t, d).MemoryLimitBytes; got != hostMemory {
		t.Fatalf("an app with an unlimited container may use the host's memory, got %d want %d", got, hostMemory)
	}
}

func TestMetricsVolumeBytesCountBindMountedData(t *testing.T) {
	web, db := numbersContainer("web", 0), numbersContainer("db", 0)
	d := numbersApps(t)
	data := filepath.Join(d.appDir(numbersApp), "data")
	if err := os.MkdirAll(filepath.Join(data, "db", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"index.html": 1000, "db/table": 2000, "db/nested/log": 3000} {
		if err := os.WriteFile(filepath.Join(data, filepath.FromSlash(name)), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	web.Mounts = []dockertest.Mount{{Type: "bind", Source: data, Destination: "/data"}}
	// Nested in the first mount: folded, never counted twice.
	db.Mounts = []dockertest.Mount{{Type: "bind", Source: filepath.Join(data, "db"), Destination: "/var/lib/db"}}
	state := dockertest.State{Containers: []dockertest.Container{web, db}}
	dockertest.Install(t, state)
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := numbersPoint(t, d).VolumeBytes
		if got == 6000 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the app's bind-mounted data reads %d bytes, want 6000", got)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestMetricsVolumeBytesCountTheProjectsNamedVolumes(t *testing.T) {
	web, db := numbersContainer("web", 0), numbersContainer("db", 0)
	project := map[string]string{"com.docker.compose.project": numbersApp}
	d := numbersFixture(t, dockertest.State{Containers: []dockertest.Container{web, db}, Volumes: []dockertest.Volume{
		{Name: numbersApp + "_cache", Labels: project, Size: "2MB"},
		// The importer's scoped name: no "<id>_" prefix.
		{Name: numbersApp + "-vol-data", Labels: project, Size: "1MB"},
		{Name: "dpl_ffff000000000001_data", Labels: map[string]string{"com.docker.compose.project": "dpl_ffff000000000001"}, Size: "5MB"},
	}})
	if got := numbersPoint(t, d).VolumeBytes; got != 3000000 {
		t.Fatalf("the project's named volumes read %d bytes, want 3000000", got)
	}
}

// Guards: these read the same before and after the fix.

func TestMetricsMemoryLimitSumsOwnLimits(t *testing.T) {
	web, db := numbersContainer("web", 256*mib), numbersContainer("db", 512*mib)
	d := numbersFixture(t, dockertest.State{Containers: []dockertest.Container{web, db}, Stats: map[string]dockertest.Stats{
		containerName(web): {CPUPerc: "1.00%", MemUsage: "100MiB / 256MiB"},
		containerName(db):  {CPUPerc: "1.00%", MemUsage: "200MiB / 512MiB"},
	}})
	if got := numbersPoint(t, d).MemoryLimitBytes; got != 768*mib {
		t.Fatalf("containers with their own limits sum to %d, want %d", got, 768*mib)
	}
}

func TestMetricsVolumeBytesIgnoreBindsOutsideTheApp(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "host-file"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	web, db := numbersContainer("web", 0), numbersContainer("db", 0)
	web.Mounts = []dockertest.Mount{{Type: "bind", Source: outside, Destination: "/host"}}
	d := numbersFixture(t, dockertest.State{Containers: []dockertest.Container{web, db}})
	for i := 0; i < 3; i++ {
		if got := numbersPoint(t, d).VolumeBytes; got != 0 {
			t.Fatalf("a bind mount outside the app's directory was counted: %d", got)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A container that mounts a directory of the app can write a link into it,
// and a mount source that is that link, or whose path goes through it,
// names a place outside the app.
func TestMetricsVolumeBytesIgnoreALinkedMountOutOfTheApp(t *testing.T) {
	linkOutOfTheApp(t, "link")
}

func TestMetricsVolumeBytesIgnoreAMountThroughALinkOutOfTheApp(t *testing.T) {
	linkOutOfTheApp(t, filepath.Join("link", "sub"))
}

func linkOutOfTheApp(t *testing.T, source string) {
	t.Helper()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sub", "host-file"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	d := numbersApps(t)
	if err := os.Symlink(outside, filepath.Join(d.appDir(numbersApp), "link")); err != nil {
		t.Skip("symbolic links unavailable here: " + err.Error())
	}
	web, db := numbersContainer("web", 0), numbersContainer("db", 0)
	web.Mounts = []dockertest.Mount{{Type: "bind", Source: filepath.Join(d.appDir(numbersApp), source), Destination: "/data"}}
	dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{web, db}})
	for i := 0; i < 10; i++ {
		if got := numbersPoint(t, d).VolumeBytes; got != 0 {
			t.Fatalf("the walk followed a link out of the app's directory: %d bytes", got)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
