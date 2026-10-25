package proxy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// A pending journal must be regenerated and reloaded before completion,
// including retries whose on-disk fragment already has only the new hostname.

func handoverRetryDropPrevious(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, blocks, err := splitFragment(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, block := range blocks {
		if strings.HasPrefix(block, "before.example.test {") {
			continue
		}
		kept = append(kept, block)
	}
	if err := writeSwitchFile(path, []byte(strings.Join(append(header, kept...), "\n")+"\n")); err != nil {
		t.Fatal(err)
	}
}

func handoverRetryCountReloadWhilePending(t *testing.T, c *Caddy) *int {
	t.Helper()
	reloads := 0
	c.switchReload = func(ctx context.Context) error {
		reloads++
		if err := c.guardRoutingSwitch(); err == nil {
			t.Fatal("journal removed before reload")
		}
		return ctx.Err()
	}
	return &reloads
}

func TestDomainHandoverRegenerateFailureRetriesWithReload(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	reloads := handoverRetryCountReloadWhilePending(t, c)
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	began := *reloads
	c.switchRegenerate = func() error { return errors.New("injected regenerate failure") }
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err == nil {
		t.Fatal("injected regenerate failure completed the handover")
	}
	if *reloads != began {
		t.Fatalf("regenerate failure still reloaded (%d -> %d)", began, *reloads)
	}
	if err := c.guardRoutingSwitch(); err == nil {
		t.Fatal("journal removed after regenerate failure")
	}
	if b, a := handoverBlocks(t, path); b != 0 || a != 1 {
		t.Fatalf("regenerate failure leaves the new fragment on disk, got before=%d after=%d", b, a)
	}
	c.switchRegenerate = nil
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal(err)
	}
	if *reloads != began+1 {
		t.Fatalf("retry must reload once before removing the journal, got %d reloads after begin's %d", *reloads, began)
	}
	if err := c.guardRoutingSwitch(); err != nil {
		t.Fatal("journal still pending after the reloaded retry:", err)
	}
}

func TestDomainHandoverCrashBetweenWriteAndReload(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	reloads := handoverRetryCountReloadWhilePending(t, c)
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	began := *reloads
	// The process died after the commit write and before regenerate/reload.
	handoverRetryDropPrevious(t, path)
	if err := c.FinishDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal(err)
	}
	if *reloads != began+1 {
		t.Fatalf("retry after the crash must reload before removing the journal, got %d reloads after begin's %d", *reloads, began)
	}
	if err := c.guardRoutingSwitch(); err != nil {
		t.Fatal("journal still pending after the reloaded retry:", err)
	}
}
