package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func domainHandoverFixture(t *testing.T) (*Caddy, string, string, []byte) {
	t.Helper()
	c := &Caddy{StateDir: t.TempDir(), switchReload: func(ctx context.Context) error { return ctx.Err() }}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	id := "dpl_" + strings.Repeat("a", 16)
	path := filepath.Join(c.StateDir, "deployments", id+".caddy")
	raw := []byte(renderFragment(id, []Route{{Hostname: "before.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return c, id, path, raw
}
func TestDomainHandoverDurableRollback(t *testing.T) {
	c, id, path, original := domainHandoverFixture(t)
	undo, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "after.example.test {") {
		t.Fatal("new domain missing")
	}
	restarted := &Caddy{StateDir: c.StateDir}
	if err := restarted.guardRoutingSwitch(); !errors.Is(err, ErrRoutingRecoveryRequired) {
		t.Fatal("restart lost pending handover")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := undo(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal("previous domain not restored exactly")
	}
	if err := c.guardRoutingSwitch(); err != nil {
		t.Fatal(err)
	}
}

func TestDomainHandoverRestartRestoresExactFragment(t *testing.T) {
	c, id, path, original := domainHandoverFixture(t)
	_, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}})
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Caddy{StateDir: c.StateDir, switchReload: func(ctx context.Context) error { return ctx.Err() }}
	if err := restarted.RestoreDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal("restart did not restore the exact route")
	}
	if err := restarted.RestoreDomainHandover(t.Context(), id, "before.example.test", "after.example.test"); err != nil {
		t.Fatal("rollback replay must verify and reload previous route:", err)
	}
	if err := restarted.RestoreDomainHandover(t.Context(), id, "foreign.example.test", "after.example.test"); err == nil {
		t.Fatal("recovery accepted an unrelated previous domain")
	}
}

func TestDomainHandoverRecoveryCannotConsumeOtherSwitch(t *testing.T) {
	c, id, _, _ := domainHandoverFixture(t)
	_, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RestoreDomainHandover(t.Context(), id, "before.example.test", "wrong.example.test"); err == nil {
		t.Fatal("wrong operation restored a journal")
	}
	if err := c.FinishDomainHandover("dpl_"+strings.Repeat("b", 16), "after.example.test"); err == nil {
		t.Fatal("wrong app deleted a journal")
	}
	if err := c.guardRoutingSwitch(); err == nil {
		t.Fatal("pending journal disappeared")
	}
}

func TestDomainHandoverFirstDomainRollback(t *testing.T) {
	c, id, path, _ := domainHandoverFixture(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := c.BeginDomainHandover(t.Context(), id, "", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RestoreDomainHandover(t.Context(), id, "", "after.example.test"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	_, blocks, err := splitFragment(string(raw))
	if err != nil || len(blocks) != 0 {
		t.Fatal("first-domain rollback left a public route")
	}
	if err := c.RestoreDomainHandover(t.Context(), id, "", "after.example.test"); err != nil {
		t.Fatal("empty-route recovery replay failed", err)
	}
}
func TestDomainHandoverReloadFailureAndRecoveryFailure(t *testing.T) {
	for _, brokenRollback := range []bool{false, true} {
		c, id, path, original := domainHandoverFixture(t)
		calls := 0
		c.switchReload = func(ctx context.Context) error {
			calls++
			if calls == 1 || brokenRollback {
				return errors.New("fixture reload refused")
			}
			return ctx.Err()
		}
		undo, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}})
		if err == nil || undo != nil {
			t.Fatal("rejected reload accepted")
		}
		got, _ := os.ReadFile(path)
		if string(got) != string(original) {
			t.Fatal("previous fragment not restored")
		}
		if errors.Is(c.guardRoutingSwitch(), ErrRoutingRecoveryRequired) != brokenRollback {
			t.Fatal("recovery journal outcome mismatch")
		}
	}
}
func TestDomainHandoverRejectsIdentityAndOnionDrift(t *testing.T) {
	for _, before := range []string{"wrong.example.test", "before.example.test\n}", "x.onion"} {
		c, id, _, _ := domainHandoverFixture(t)
		if _, err := c.BeginDomainHandover(t.Context(), id, before, "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80"}}); err == nil {
			t.Fatal("unreviewed identity accepted")
		}
		if err := c.guardRoutingSwitch(); err != nil {
			t.Fatal("invalid request wrote journal")
		}
	}
	c, id, _, _ := domainHandoverFixture(t)
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", OnionAddr: strings.Repeat("a", 56) + ".onion", Upstream: id + "-app:80"}}); err == nil {
		t.Fatal("onion endpoint changed during domain handover")
	}
}
