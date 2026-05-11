package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadMissingReturnsErrNoConfig(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "does-not-exist.toml")

	_, err := LoadFromPath(path)
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("want ErrNoConfig, got %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	in := NewEmpty()
	if err := in.Add("personal", Context{Key: "imp_a1b2c3d4", Secret: "s3cr3t"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := in.Add("ci-bot", Context{Key: "imp_z9y8x7w6", Secret: "ci-only", UseTor: true}); err != nil {
		t.Fatalf("Add ci-bot: %v", err)
	}

	if err := in.SaveToPath(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if out.Default != "personal" {
		t.Errorf("Default = %q, want personal (first-added auto-default)", out.Default)
	}
	if len(out.Contexts) != 2 {
		t.Errorf("len(Contexts) = %d, want 2", len(out.Contexts))
	}
	if out.Contexts["personal"].Key != "imp_a1b2c3d4" {
		t.Errorf("personal.Key = %q, want imp_a1b2c3d4", out.Contexts["personal"].Key)
	}
	if !out.Contexts["ci-bot"].UseTor {
		t.Errorf("ci-bot.UseTor = false, want true")
	}
}

func TestActiveResolvesOverrideThenDefault(t *testing.T) {
	c := NewEmpty()
	_ = c.Add("personal", Context{Key: "imp_111"})
	_ = c.Add("ci-bot", Context{Key: "imp_222"})

	// No override → falls back to Default (personal, set by first Add).
	name, ctx, err := c.Active("")
	if err != nil {
		t.Fatalf("Active(\"\"): %v", err)
	}
	if name != "personal" || ctx.Key != "imp_111" {
		t.Errorf("Active(\"\") = (%q, %q), want (personal, imp_111)", name, ctx.Key)
	}

	// Override wins over Default.
	name, ctx, err = c.Active("ci-bot")
	if err != nil {
		t.Fatalf("Active(ci-bot): %v", err)
	}
	if name != "ci-bot" || ctx.Key != "imp_222" {
		t.Errorf("Active(ci-bot) = (%q, %q), want (ci-bot, imp_222)", name, ctx.Key)
	}

	// Override naming a context that doesn't exist → ErrContextNotFound.
	if _, _, err := c.Active("nope"); !errors.Is(err, ErrContextNotFound) {
		t.Errorf("Active(nope): want ErrContextNotFound, got %v", err)
	}
}

func TestActiveErrNoDefaultWhenEmpty(t *testing.T) {
	c := NewEmpty()
	if _, _, err := c.Active(""); !errors.Is(err, ErrNoDefaultContext) {
		t.Errorf("want ErrNoDefaultContext, got %v", err)
	}
}

func TestAddDuplicateIsRejected(t *testing.T) {
	c := NewEmpty()
	_ = c.Add("personal", Context{Key: "imp_a"})
	if err := c.Add("personal", Context{Key: "imp_b"}); !errors.Is(err, ErrDuplicateContext) {
		t.Errorf("want ErrDuplicateContext, got %v", err)
	}
}

func TestDeleteClearsDefaultIfMatched(t *testing.T) {
	c := NewEmpty()
	_ = c.Add("personal", Context{Key: "imp_a"})
	_ = c.Add("ci-bot", Context{Key: "imp_b"})
	// personal is default (first added).

	if err := c.Delete("personal"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if c.Default != "" {
		t.Errorf("Default = %q, want empty after deleting the default context", c.Default)
	}
	if _, exists := c.Contexts["personal"]; exists {
		t.Errorf("personal should be removed")
	}
	if _, exists := c.Contexts["ci-bot"]; !exists {
		t.Errorf("ci-bot should still exist")
	}
}

func TestSetDefaultRequiresExistingContext(t *testing.T) {
	c := NewEmpty()
	_ = c.Add("personal", Context{Key: "imp_a"})

	if err := c.SetDefault("nope"); !errors.Is(err, ErrContextNotFound) {
		t.Errorf("want ErrContextNotFound, got %v", err)
	}
	if err := c.SetDefault("personal"); err != nil {
		t.Errorf("SetDefault(personal): %v", err)
	}
}

func TestSavePosixChmod0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACL handling differs; chmod is a no-op on win32")
	}
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")

	c := NewEmpty()
	_ = c.Add("personal", Context{Key: "imp_a"})

	if err := c.SaveToPath(path); err != nil {
		t.Fatalf("SaveToPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config perm = %#o, want 0600", info.Mode().Perm())
	}
}

func TestConfigPathRespectsImprezaConfigOverride(t *testing.T) {
	tmp := t.TempDir()
	want := filepath.Join(tmp, "custom-location.toml")
	t.Setenv("IMPREZA_CONFIG", want)

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
}
