package executor

// Startup gate v2 tests: a service that declares no healthcheck is ready
// once the settle gate has seen it stable, like `docker compose up --wait`;
// a service that declares one still has to reach healthy.

import (
	"testing"
	"time"
)

func TestStartupV2ServiceWithoutHealthcheckIsReadyWhenStable(t *testing.T) {
	states := []containerState{{Name: "app", Status: "running"}}
	// Advisory: always accepted.
	if !startupStatesOKProtocol(states, false, true) {
		t.Fatal("advisory gate refused a running service without a healthcheck")
	}
	// Required (v2): running + the settle gate's stability is enough.
	if !startupStatesOKProtocol(states, true, true) {
		t.Fatal("the required gate refused a running service that declares no healthcheck (v2: compose up --wait semantics)")
	}
	// The receipt says the v2 protocol.
	policy := startupPolicy{RequireHealthy: true, Timeout: 30 * time.Second, V2: true, Protocol: "startup-health-v2"}
	if r := policy.receipt(settleHealthy); r == nil || r.Protocol != "startup-health-v2" {
		t.Fatalf("receipt protocol drifted: %+v", r)
	}
}

func TestStartupV2ServiceWithoutHealthcheckNeverReadyWhileRestarting(t *testing.T) {
	states := []containerState{{Name: "app", Status: "restarting"}}
	if startupStatesOKProtocol(states, false, true) || startupStatesOKProtocol(states, true, true) {
		t.Fatal("a restarting service was classified ready")
	}
	// The settle gate also convicts it through the streak path.
	exited := []containerState{{Name: "app", Status: "exited", ExitCode: 1}}
	if startupStatesOK(exited, true) {
		t.Fatal("a crash-looping service was classified ready")
	}
}

func TestStartupV2ServiceWithHealthcheckRequiresHealthy(t *testing.T) {
	// healthy passes; starting and unhealthy hold the gate.
	healthy := []containerState{{Name: "app", Status: "running", Health: "healthy"}}
	if !startupStatesOKProtocol(healthy, true, true) {
		t.Fatal("a healthy service was refused")
	}
	starting := []containerState{{Name: "app", Status: "running", Health: "starting"}}
	if startupStatesOKProtocol(starting, true, true) {
		t.Fatal("a starting healthcheck was accepted as ready")
	}
	unhealthy := []containerState{{Name: "app", Status: "running", Health: "unhealthy"}}
	if startupStatesOKProtocol(unhealthy, true, true) {
		t.Fatal("an unhealthy service was accepted as ready")
	}
	// A mixed stack: the sidecar without a healthcheck is fine alongside
	// a healthy main service.
	mixed := []containerState{
		{Name: "app", Status: "running", Health: "healthy"},
		{Name: "sidecar", Status: "running"},
	}
	if !startupStatesOKProtocol(mixed, true, true) {
		t.Fatal("a healthy service with a no-healthcheck sidecar was refused")
	}
}
