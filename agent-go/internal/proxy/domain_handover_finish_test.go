package proxy

import (
	"context"
	"os"
	"strings"
	"testing"
)

func handoverBlocks(t *testing.T, path string) (before, after int) {
	t.Helper()
	raw, _ := os.ReadFile(path)
	_, blocks, err := splitFragment(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if strings.HasPrefix(b, "before.example.test {") {
			before++
		}
		if strings.HasPrefix(b, "after.example.test {") {
			after++
		}
	}
	return
}

// the commit drops the previous block only after the probe, reloads once, and is idempotent.
func TestDomainHandoverFinishDropsPreviousOnceAndIsIdempotent(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	reloads := 0
	c.switchReload = func(ctx context.Context) error { reloads++; return ctx.Err() }
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal(err)
	}
	if b, a := handoverBlocks(t, path); b != 0 || a != 1 {
		t.Fatalf("after the commit only the new domain serves, got before=%d after=%d", b, a)
	}
	if reloads != 2 {
		t.Fatalf("overlap + commit = 2 reloads, got %d", reloads)
	}
	if err := c.guardRoutingSwitch(); err != nil {
		t.Fatal("journal not completed:", err)
	}
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil || reloads != 2 {
		t.Fatalf("a replayed commit must be a no-op without reload (err=%v reloads=%d)", err, reloads)
	}
}

// Crash between the durable "switched" receipt and the commit: a restarted agent commits from the overlap.
func TestDomainHandoverFinishAfterRestartCommitsFromOverlap(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	restarted := &Caddy{StateDir: c.StateDir, switchReload: func(ctx context.Context) error { return ctx.Err() }}
	if err := restarted.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal(err)
	}
	if b, a := handoverBlocks(t, path); b != 0 || a != 1 {
		t.Fatalf("restart commit left before=%d after=%d", b, a)
	}
}

// A failed commit reload keeps the journal pending (recovery required), never a half state reported as done.
func TestDomainHandoverFinishReloadFailureKeepsJournal(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	calls := 0
	c.switchReload = func(ctx context.Context) error {
		calls++
		if calls == 2 {
			return context.DeadlineExceeded
		}
		return ctx.Err()
	}
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err == nil {
		t.Fatal("a failed commit reload was reported as finished")
	}
	if err := c.guardRoutingSwitch(); err == nil {
		t.Fatal("journal completed after a failed commit")
	}
	if b, a := handoverBlocks(t, path); b != 1 || a != 1 {
		t.Fatalf("a failed commit must leave the overlap on disk (what Caddy still runs), got before=%d after=%d", b, a)
	}
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal("retried commit:", err)
	}
	if b, a := handoverBlocks(t, path); b != 0 || a != 1 || calls != 3 {
		t.Fatalf("retried commit must drop and reload again, got before=%d after=%d reloads=%d", b, a, calls)
	}
}

// First clearnet domain: nothing to keep serving, so Begin publishes the candidate and Finish does not reload.
func TestDomainHandoverFirstDomainFinishNoExtraReload(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	c.switchReload = func(ctx context.Context) error { reloads++; return ctx.Err() }
	if _, err := c.BeginDomainHandover(t.Context(), id, "", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.FinishDomainHandover(t.Context(), id, "", "after.example.test"); err != nil || reloads != 1 {
		t.Fatalf("first domain: err=%v reloads=%d", err, reloads)
	}
}
