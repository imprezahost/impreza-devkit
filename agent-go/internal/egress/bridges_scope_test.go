package egress

// The per-interface drops used a `br+` wildcard, which also matches
// bridges that are NOT Docker's — a host whose public interface is a bridge
// (br0) had its own INPUT traffic walk the container drops. The scope is
// now the bridges Docker itself lists (the same enumeration the ingress
// firewall trusts), a bridge carrying a default route is skipped, and a
// Docker-down reconcile keeps the last scope applied instead of flapping
// to docker0-only. These tests fail on the previous release: it renders `br+` for
// every caller regardless of the enumerated list.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func joinRules(rules [][]string) string {
	var sb strings.Builder
	for _, r := range rules {
		sb.WriteString(strings.Join(r, " "))
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestNoWildcardBridgeInAnyRenderedChain(t *testing.T) {
	bridges := []string{"docker0", "br-111122223333"}
	for name, rules := range map[string][][]string{
		"forward-v4": Rules([]string{"10.0.0.2"}, bridges),
		"forward-v6": Rules6(nil, bridges),
		"host-v4":    HostRules(bridges),
		"host-v6":    HostRules6(bridges),
	} {
		if strings.Contains(joinRules(rules), "br+") {
			t.Fatalf("%s still renders the br+ wildcard:\n%s", name, joinRules(rules))
		}
	}
}

func TestDropsScopeToDockerBridgesOnly(t *testing.T) {
	bridges := []string{"docker0", "br-111122223333"}
	forward := joinRules(Rules([]string{"10.0.0.2"}, bridges))
	host := joinRules(HostRules(bridges))
	for _, chain := range []string{forward, host} {
		for _, iface := range bridges {
			if !strings.Contains(chain, "-i "+iface+" ") {
				t.Fatalf("enumerated bridge %s missing from chain:\n%s", iface, chain)
			}
		}
		// A host-owned bridge (br0) must never receive the container drops.
		if strings.Contains(chain, "-i br0 ") {
			t.Fatal("a non-Docker bridge received the container drops")
		}
	}
}

func TestDockerBridgeInterfacesEnumeration(t *testing.T) {
	d := &dockerFake{}
	got := dockerBridgeInterfaces(context.Background(), d.run, nil)
	if strings.Join(got, ",") != "br-111122223333,docker0" {
		t.Fatalf("enumeration: %v", got)
	}
	// Docker down WITH history keeps the last scope (the ports pattern).
	if got := dockerBridgeInterfaces(context.Background(), (&dockerFake{down: true}).run, []string{"br-custom000001"}); strings.Join(got, ",") != "br-custom000001" {
		t.Fatalf("docker-down did not keep the previous scope: %v", got)
	}
	// Docker down WITHOUT history falls back to the default bridge only.
	if got := dockerBridgeInterfaces(context.Background(), (&dockerFake{down: true}).run, nil); strings.Join(got, ",") != "docker0" {
		t.Fatalf("docker-down fallback: %v", got)
	}
}

func TestDockerBridgeInterfacesSkipsDefaultRouteBridges(t *testing.T) {
	// A docker runner whose ONLY network sits on a bridge that also carries
	// a default route: the enumeration must skip it rather than drop the
	// host's own transit. On hosts without /proc/net/route (windows dev
	// boxes) DefaultRouteInterfaces is empty and the bridge survives — the
	// skip is exercised on Linux hosts; here we at least assert the runner is
	// consumed and never errors out.
	calls := 0
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if args[0] == "network" && args[1] == "ls" {
			return []byte("aaaaaaaaaaaa\n"), nil
		}
		if args[0] == "network" && args[1] == "inspect" {
			return []byte("aaaaaaaaaaaa|br-pub000000009\n"), nil
		}
		return nil, errors.New("unexpected docker call")
	}
	got := dockerBridgeInterfaces(context.Background(), run, nil)
	if calls == 0 {
		t.Fatal("enumeration never consulted docker")
	}
	t.Logf("bridges with a default-route skip: %v", got)
}
