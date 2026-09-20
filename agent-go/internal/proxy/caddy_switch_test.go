package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSwitchRestoresAfterReloadFailureAndCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "cancelled"}[cancelRequest], func(t *testing.T) {
			c := &Caddy{StateDir: t.TempDir()}
			if err := c.ensureDirs(); err != nil {
				t.Fatal(err)
			}
			source, target := "dpl_"+strings.Repeat("a", 16), "dpl_"+strings.Repeat("b", 16)
			path := filepath.Join(c.StateDir, "deployments", source+".caddy")
			original := []byte("app.example.test {\n  reverse_proxy " + source + "-app:8080\n}\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			c.switchReload = func(ctx context.Context) error {
				calls++
				if calls == 1 {
					if cancelRequest {
						cancel()
					}
					return errors.New("injected reload failure")
				}
				return ctx.Err()
			}
			if undo, err := c.SwitchHostname(ctx, "app.example.test", source, target, target+"-app:8080"); err == nil || undo != nil {
				t.Fatal("failed switch accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(original) {
				t.Fatal("source not restored")
			}
			if _, err = os.Stat(filepath.Join(c.StateDir, "deployments", target+".caddy")); !os.IsNotExist(err) {
				t.Fatal("target fragment left behind")
			}
			if calls != 2 {
				t.Fatalf("recovery reloads=%d", calls)
			}
		})
	}
}

func TestSwitchRollbackSurvivesCallerCancellation(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir(), switchReload: func(ctx context.Context) error { return ctx.Err() }}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	source, target := "dpl_"+strings.Repeat("a", 16), "dpl_"+strings.Repeat("b", 16)
	p := filepath.Join(c.StateDir, "deployments", source+".caddy")
	if err := os.WriteFile(p, []byte("app.example.test {\n  reverse_proxy "+source+"-app:8080\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	undo, err := c.SwitchHostname(t.Context(), "app.example.test", source, target, target+"-app:8080")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := undo(ctx); err != nil {
		t.Fatalf("cancelled recovery: %v", err)
	}
}

func TestSplitFragment(t *testing.T) {
	raw := "# deployment dpl_a\napp.example.test {\n  tls ops@example.test\n  reverse_proxy dpl_a-app:8080\n}\n\nhttp://abc.onion {\n  reverse_proxy dpl_a-app:8080\n}\n"
	header, blocks, err := splitFragment(raw)
	if err != nil || len(header) != 1 || len(blocks) != 2 {
		t.Fatalf("fragment split failed: %v %v %v", header, blocks, err)
	}
	if !strings.HasPrefix(blocks[0], "app.example.test {") || !strings.HasPrefix(blocks[1], "http://abc.onion {") {
		t.Fatal("fragment blocks misidentified")
	}
	if _, _, err := splitFragment("# h\napp.example.test {\n  reverse_proxy x\n"); err == nil {
		t.Fatal("unterminated block accepted")
	}
	if _, _, err := splitFragment("  app.example.test {\n}\n"); err == nil {
		t.Fatal("indented block opening accepted")
	}
	if header, blocks, err := splitFragment(""); err != nil || len(header) != 0 || len(blocks) != 0 {
		t.Fatal("empty fragment must split empty")
	}
}

func TestSwitchHostnameRefusals(t *testing.T) {
	dir := t.TempDir()
	c := &Caddy{StateDir: filepath.Join(dir, "proxy")}
	source := "dpl_" + strings.Repeat("a", 16)
	target := "dpl_" + strings.Repeat("b", 16)
	hostname := "app.example.test"
	if err := os.MkdirAll(filepath.Join(c.StateDir, "deployments"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(id, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.StateDir, "deployments", id+".caddy"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// The source must be serving the reviewed hostname.
	write(source, "# deployment "+source+"\nother.example.test {\n  reverse_proxy "+source+"-app:8080\n}\n")
	if _, err := c.SwitchHostname(t.Context(), hostname, source, target, target+"-app:8080"); err == nil {
		t.Fatal("switch accepted a source that is not serving the hostname")
	}
	// A hostname block with two upstreams is never guessed at.
	write(source, hostname+" {\n  reverse_proxy "+source+"-app:8080\n  reverse_proxy "+source+"-app:8081\n}\n")
	if _, err := c.SwitchHostname(t.Context(), hostname, source, target, target+"-app:8080"); err == nil {
		t.Fatal("switch accepted a block with two upstreams")
	}
	// And one with none.
	write(source, hostname+" {\n  tls ops@example.test\n}\n")
	if _, err := c.SwitchHostname(t.Context(), hostname, source, target, target+"-app:8080"); err == nil {
		t.Fatal("switch accepted a block with no upstream")
	}
	// A malformed target fragment refuses rather than rewriting around it.
	write(source, hostname+" {\n  reverse_proxy "+source+"-app:8080\n}\n")
	write(target, "broken {\n  reverse_proxy x\n")
	if _, err := c.SwitchHostname(t.Context(), hostname, source, target, target+"-app:8080"); err == nil {
		t.Fatal("switch accepted an unverifiable target fragment")
	}
}
func TestSwitchReportsFailedRecovery(t *testing.T) {
	c := &Caddy{StateDir: t.TempDir(), switchReload: func(context.Context) error { return errors.New("reload unavailable") }}
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	source, target := "dpl_"+strings.Repeat("a", 16), "dpl_"+strings.Repeat("b", 16)
	path := filepath.Join(c.StateDir, "deployments", source+".caddy")
	if err := os.WriteFile(path, []byte("app.example.test {\n  reverse_proxy "+source+"-app:8080\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := c.SwitchHostname(t.Context(), "app.example.test", source, target, target+"-app:8080")
	if !errors.Is(err, ErrRoutingRecoveryRequired) {
		t.Fatalf("missing recovery classification: %v", err)
	}
}
