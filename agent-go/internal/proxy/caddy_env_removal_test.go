package proxy

// A variable that leaves the env-file must leave the
// proxy container too. The old compare only checked that every wanted
// entry was present with the right value, so a removed variable — for
// example a stale agent credential after a re-bootstrap, or anything an
// operator deletes — lingered in the container's environment, which is
// exposed to the internet, and no recreation ever picked the removal up.
// These tests drive EnsureRunning against the docker double.

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestEnsureRunningRecreatesWhenAVariableLeavesTheEnvFile(t *testing.T) {
	c, d, logs := newProxyDouble(t)
	ctx := context.Background()
	if err := c.SetImprezaCredentials(ctx, "agt_stale", "stale-secret-a", "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	first := proxyContainer(t, d)

	// An operator-level entry joins the file: the proxy is recreated and
	// the entry reaches the container (the value-add direction already
	// worked).
	envFile := c.envFilePath()
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, append(append([]byte{}, raw...), []byte("STALE_EXTRA=leave\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	second := proxyContainer(t, d)
	if second.ID == first.ID || envValue(second.Config.Env, "STALE_EXTRA") != "leave" {
		t.Fatal("the added variable never reached a recreated proxy")
	}

	// The entry leaves the file: the proxy must be recreated WITHOUT it.
	if err := os.WriteFile(envFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	third := proxyContainer(t, d)
	if third.ID == second.ID {
		t.Fatal("a variable removed from the env-file stayed in the proxy: a stale environment exposed to the internet")
	}
	if got := envValue(third.Config.Env, "STALE_EXTRA"); got != "" {
		t.Fatalf("the removed variable is still in the container environment: %q", got)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if runs, _ := proxyLaunches(d); runs != 3 {
		t.Fatalf("expected exactly three launches (start, add, removal), got %d", runs)
	}
	if strings.Contains(logs.String(), "stale-secret") {
		t.Fatal("the proxy lifecycle logged a credential")
	}
}
