package proxy

// Coverage gaps in the proxy environment check:
//   - The agent's credential leaving the env-file (the agent back in "no
//     platform" mode) was only covered by the unit table of proxyEnvMatches,
//     with a placeholder value; no EnsureRunning test removed it.
//   - When `docker image inspect` fails, or answers something that is not
//     the image's environment, the image defaults are unknown. The check
//     must then fail closed: an entry it cannot account for forces the
//     recreation, so a stale credential never stays in the proxy. Nothing
//     covered that branch, and a fail-open check passed every test.
//   - A failed inspect must not be cached: once the image answers again,
//     a matching proxy is kept.
// These drive EnsureRunning against the docker double.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
)

// The secret's value, not only its name, must leave the container.
const envCheckSecret = "envcheck-secret-old-value"

func envCheckHas(env []string, needle string) bool {
	for _, entry := range env {
		if strings.Contains(entry, needle) {
			return true
		}
	}
	return false
}

func envCheckImageInspects(d *dockertest.Double) int {
	n := 0
	for _, call := range d.Calls("image") {
		if len(call.Args) > 1 && call.Args[1] == "inspect" {
			n++
		}
	}
	return n
}

// envCheckCredentialLeaves starts the proxy with the agent's credential, then
// clears it from the env-file and runs EnsureRunning again. It returns the
// container from before and after the removal.
func envCheckCredentialLeaves(t *testing.T, c *Caddy, d *dockertest.Double) (before, after dockertest.Container) {
	t.Helper()
	ctx := context.Background()
	if err := c.SetImprezaCredentials(ctx, "agt_envcheckold", envCheckSecret, "https://api.example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	before = proxyContainer(t, d)
	if envValue(before.Config.Env, ImprezaAgentSecretEnvVar) != envCheckSecret {
		t.Fatal("setup: the proxy never got the credential")
	}
	if err := c.SetImprezaCredentials(ctx, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	return before, proxyContainer(t, d)
}

func envCheckAssertCredentialGone(t *testing.T, env []string) {
	t.Helper()
	for _, needle := range []string{envCheckSecret, "agt_envcheckold", ImprezaAgentSecretEnvVar + "=",
		ImprezaAgentIDEnvVar + "=", ImprezaAPIURLEnvVar + "="} {
		if envCheckHas(env, needle) {
			t.Errorf("still in the proxy's environment after the credential left the env-file: %q", needle)
		}
	}
}

func TestEnsureRunningDropsARemovedCredentialWithItsValue(t *testing.T) {
	c, d, logs := newProxyDouble(t)
	before, after := envCheckCredentialLeaves(t, c, d)
	if after.ID == before.ID {
		t.Fatal("the credential left the env-file and the serving proxy was kept, secret included")
	}
	envCheckAssertCredentialGone(t, after.Config.Env)
	// The image's own environment is still there, and the next call keeps
	// the proxy: two launches, the start and the removal.
	if envValue(after.Config.Env, "PATH") == "" {
		t.Fatal("the recreated proxy lost the image's PATH")
	}
	if err := c.EnsureRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs, _ := proxyLaunches(d); runs != 2 {
		t.Fatalf("expected two launches (start, removal), got %d", runs)
	}
	if strings.Contains(logs.String(), "envcheck-secret") {
		t.Fatal("the proxy lifecycle logged a credential")
	}
}

func TestEnsureRunningWithoutTheImageDefaultsFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state dockertest.State
	}{
		// The image reference is not on the host: the inspect fails.
		{"image inspect fails", dockertest.State{Networks: []string{NetworkName}, MissingImages: []string{Image}}},
		// The inspect answers, but not with a list of entries.
		{"image inspect unreadable", dockertest.State{Networks: []string{NetworkName}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dockertest.Install(t, tc.state)
			logs := &bytes.Buffer{}
			c := New(t.TempDir(), slog.New(slog.NewTextHandler(logs, nil)))
			before, after := envCheckCredentialLeaves(t, c, d)
			if after.ID == before.ID {
				t.Fatal("without the image defaults the stale credential was kept in the proxy (fail-open)")
			}
			envCheckAssertCredentialGone(t, after.Config.Env)
			if strings.Contains(logs.String(), "envcheck-secret") {
				t.Fatal("the proxy lifecycle logged a credential")
			}
		})
	}
}

func TestEnsureRunningDoesNotCacheAFailedImageInspect(t *testing.T) {
	d := dockertest.Install(t, dockertest.State{Networks: []string{NetworkName}, MissingImages: []string{Image}})
	c := New(t.TempDir(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ctx := context.Background()
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	failing := envCheckImageInspects(d)
	if failing == 0 {
		t.Fatal("setup: the image defaults were never asked for")
	}

	// The image answers again, with its environment.
	state := d.State()
	state.MissingImages = nil
	state.ImageEnv = map[string][]string{Image: {"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}}
	d.Save(state)
	runsBefore, _ := proxyLaunches(d)
	for i := 0; i < 3; i++ {
		if err := c.EnsureRunning(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if envCheckImageInspects(d) == failing {
		t.Fatal("the failed inspect was cached: the image defaults were never asked for again")
	}
	runs, _ := proxyLaunches(d)
	if runs != runsBefore {
		t.Fatalf("with the image answering again the matching proxy is still recreated: %d launches in 3 calls", runs-runsBefore)
	}
}
