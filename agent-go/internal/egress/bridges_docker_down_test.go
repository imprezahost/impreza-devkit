package egress

// Apply and Apply6 take the Docker-down fallback from the
// status they recorded (readStatus(dir).Bridges / readStatus6(dir).Bridges).
// These tests drive that exact sequence through the real status file: one
// reconcile with Docker answering, then one with Docker down. The FORWARD
// drops for the custom bridge must survive the second reconcile.

import (
	"strings"
	"testing"
)

func chainHasIface(rules [][]string, iface string) bool {
	for _, r := range rules {
		if len(r) >= 2 && r[0] == "-i" && r[1] == iface {
			return true
		}
	}
	return false
}

func TestForwardV4KeepsBridgeScopeWhenDockerIsDown(t *testing.T) {
	ctx, fake, resolv, dir := applyFixture(t)
	up := (&dockerFake{}).run
	down := (&dockerFake{down: true}).run

	// Reconcile 1, Docker answering: the same two lines Apply runs.
	bridges := dockerBridgeInterfaces(ctx, up, readStatus(dir).Bridges)
	if err := applyAll(ctx, fake.run, resolv, dir, bridges); err != nil {
		t.Fatal(err)
	}
	if !chainHasIface(fake.chains[Chain], "br-111122223333") {
		t.Fatalf("first apply did not scope the custom bridge: %v", fake.chains[Chain])
	}
	if got := strings.Join(readStatus(dir).Bridges, ","); got != "br-111122223333,docker0" {
		t.Errorf("v4 FORWARD status did not record the scope applied: %q", got)
	}

	// Reconcile 2, Docker not answering.
	bridges = dockerBridgeInterfaces(ctx, down, readStatus(dir).Bridges)
	if err := applyAll(ctx, fake.run, resolv, dir, bridges); err != nil {
		t.Fatal(err)
	}
	if !chainHasIface(fake.chains[Chain], "br-111122223333") {
		t.Fatalf("Docker-down reconcile dropped the custom bridge from the v4 FORWARD chain (scope now %v)", bridges)
	}
}

func TestForwardV6KeepsBridgeScopeWhenDockerIsDown(t *testing.T) {
	ctx, fake, resolv, dir := applyFixture(t)
	up := (&dockerFake{}).run
	down := (&dockerFake{down: true}).run

	bridges := dockerBridgeInterfaces(ctx, up, readStatus6(dir).Bridges)
	if err := applyAll6(ctx, fake.run, resolv, dir, bridges); err != nil {
		t.Fatal(err)
	}
	bridges = dockerBridgeInterfaces(ctx, down, readStatus6(dir).Bridges)
	if err := applyAll6(ctx, fake.run, resolv, dir, bridges); err != nil {
		t.Fatal(err)
	}
	// The v6 FORWARD chain is physdev-scoped (no per-interface drops); the
	// control is that its status keeps the scope across the Docker-down run.
	if got := strings.Join(readStatus6(dir).Bridges, ","); got != "br-111122223333,docker0" {
		t.Fatalf("v6 status lost the scope across a Docker-down reconcile: %q", got)
	}
}
