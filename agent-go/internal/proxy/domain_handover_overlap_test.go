package proxy

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Negative control: this file uses the existing transition and fixture, so it runs unchanged on the
// previous release, where it fails (the previous domain is removed from the live fragment before the new
// certificate is proven). With the fix the previous block keeps serving next to the new one.
func TestDomainHandoverKeepsPreviousDomainServingDuringProbe(t *testing.T) {
	c, id, path, original := domainHandoverFixture(t)
	reloads := 0
	c.switchReload = func(ctx context.Context) error { reloads++; return ctx.Err() }
	if _, err := c.BeginDomainHandover(t.Context(), id, "before.example.test", "after.example.test", []Route{{Hostname: "after.example.test", Upstream: id + "-app:80", TLSMode: "letsencrypt"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	_, blocks, err := splitFragment(string(got))
	if err != nil {
		t.Fatal(err)
	}
	before, after := 0, 0
	for _, b := range blocks {
		if strings.HasPrefix(b, "before.example.test {") {
			before++
		}
		if strings.HasPrefix(b, "after.example.test {") {
			after++
		}
	}
	if before != 1 || after != 1 {
		t.Fatalf("live fragment during the probe must serve both domains, got before=%d after=%d", before, after)
	}
	if !strings.HasPrefix(string(got), strings.TrimRight(string(original), "\n")) {
		t.Fatal("the previous block must be kept byte for byte")
	}
	if reloads != 1 {
		t.Fatalf("one reload to publish the overlap, got %d", reloads)
	}
}
