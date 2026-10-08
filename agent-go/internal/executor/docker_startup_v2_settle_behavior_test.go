package executor

// The v2 settle behavior against the REAL settle loop and the docker
// double. These also run on the pre-fix code, where they fail:
// a single restart followed by stability must become ready one window
// after the restart — the pre-fix forms never let it through (the fixed
// baseline re-arms forever), and the mixed smoke covers the mixed stack.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
)

// TestStartupV2SingleRestartReal: the restart-once shape — a
// service that restarts once around 4 s and then stays up becomes ready
// about one window after the restart, and the whole settle honors the
// window on the clock.
func TestStartupV2SingleRestartReal(t *testing.T) {
	original := v2StableWindow
	v2StableWindow = 30 * time.Second
	t.Cleanup(func() { v2StableWindow = original })
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := "dpl_v2win000000000a6"
	double := dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{dockertest.ComposeContainer(id, "web", "running", 0)}})
	go func() {
		time.Sleep(4 * time.Second)
		bumpRestarts(t, double.StatePath(), id)
	}()
	started := time.Now()
	verdict, why := d.awaitStackSettledPolicy(context.Background(), id, startupPolicy{RequireHealthy: true, V2: true, Timeout: 90 * time.Second})
	elapsed := time.Since(started)
	if verdict != settleHealthy {
		t.Fatalf("a single restart followed by stability never became ready: %v %s", verdict, why)
	}
	// One window since the restart, with a small tolerance for the bump
	// landing between samples.
	if elapsed < v2StableWindow+3*time.Second {
		t.Fatalf("ready after %s; the window since the restart was not honored", elapsed)
	}
	if elapsed > v2StableWindow+20*time.Second {
		t.Fatalf("ready after %s; far beyond one window past the restart", elapsed)
	}
}

// TestStartupV2MixedStackSmoke: the mixed stack against the docker double
// — a healthy checked service beside a restarting unchecked one stays not
// ready while the unchecked one flaps, and settles once it stops.
func TestStartupV2MixedStackSmoke(t *testing.T) {
	original := v2StableWindow
	v2StableWindow = 9 * time.Second
	t.Cleanup(func() { v2StableWindow = original })
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := "dpl_v2win000000000a4"
	main := dockertest.ComposeContainer(id, "web", "running", 0)
	main.State.Health = &dockertest.Health{Status: "healthy"}
	side := dockertest.ComposeContainer(id, "side", "running", 0)
	double := dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{main, side}})
	statePath := double.StatePath()
	stopFlap := make(chan struct{})
	flapDone := make(chan struct{})
	go func() {
		defer close(flapDone)
		for {
			select {
			case <-stopFlap:
				return
			case <-time.After(4 * time.Second):
				bumpRestarts(t, statePath, id)
			}
		}
	}()
	verdict, _ := d.awaitStackSettledPolicy(context.Background(), id, startupPolicy{RequireHealthy: true, V2: true, Timeout: 20 * time.Second})
	close(stopFlap)
	<-flapDone
	if verdict == settleHealthy {
		t.Fatal("a mixed stack with a restarting unchecked service was called ready")
	}
	verdict, why := d.awaitStackSettledPolicy(context.Background(), id, startupPolicy{RequireHealthy: true, V2: true, Timeout: 30 * time.Second})
	if verdict != settleHealthy {
		t.Fatalf("the settled mixed stack did not become ready: %v %s", verdict, why)
	}
}
