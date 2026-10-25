package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reload that closes a pending handover has to be the file that no longer
// serves the previous host. Counting the reload call still passes if that
// call happens before regenerate, or if regenerate is skipped and Caddy
// reloads the overlap it was already running.
func handoverRetryReloadSeesOnlyNewHost(t *testing.T, c *Caddy) *int {
	t.Helper()
	reloads := 0
	c.switchReload = func(ctx context.Context) error {
		reloads++
		if err := c.guardRoutingSwitch(); err == nil {
			t.Fatal("journal removed before reload")
		}
		raw, err := os.ReadFile(filepath.Join(c.StateDir, "Caddyfile"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "before.example.test {") {
			t.Fatalf("reload %d still serves the previous host", reloads)
		}
		if !strings.Contains(string(raw), "after.example.test {") {
			t.Fatalf("reload %d lost the new host", reloads)
		}
		return ctx.Err()
	}
	return &reloads
}

func TestDomainHandoverCrashReloadDropsPreviousHostFromFile(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	handoverRetryDropPrevious(t, path)
	reloads := handoverRetryReloadSeesOnlyNewHost(t, c)
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal(err)
	}
	if *reloads != 1 {
		t.Fatalf("the retry reload must be the one that drops the previous host, got %d", *reloads)
	}
	if err := c.guardRoutingSwitch(); err != nil {
		t.Fatal(err)
	}
}
