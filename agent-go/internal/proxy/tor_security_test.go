package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnionAuthFailureKeepsPreviousKeys(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tor.SetOnionClients(ctx, "dpl_private", map[string]string{"alice": testPubkey}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "authorized_clients", "alice.auth"))
	if err != nil {
		t.Fatal(err)
	}
	tor.reloadFn = func(context.Context) error { return fmt.Errorf("injected reload failure") }
	for _, clients := range []map[string]string{{"bob": testPubkey}, {}} {
		if err := tor.SetOnionClients(ctx, "dpl_private", clients); err == nil {
			t.Fatal("reload failure accepted")
		}
		actual, err := os.ReadFile(filepath.Join(dir, "authorized_clients", "alice.auth"))
		if err != nil || string(actual) != string(original) {
			t.Fatalf("private key policy lost: %v", err)
		}
		names, err := tor.OnionClientNames("dpl_private")
		if err != nil || len(names) != 1 || names[0] != "alice" {
			t.Fatalf("policy changed: %v %v", names, err)
		}
		if _, err := os.Stat(tor.policyPath()); err != nil {
			t.Fatal("failed reload lost recovery journal")
		}
		tor.reloadFn = func(context.Context) error { return nil }
		if err := tor.RecoverOnionPolicy(ctx); err != nil {
			t.Fatal(err)
		}
		tor.reloadFn = func(context.Context) error { return fmt.Errorf("injected reload failure") }
	}
}

func TestOnionAuthRejectsLowOrderAndNoncanonicalKeys(t *testing.T) {
	tor := newTestTor(t)
	if err := os.MkdirAll(filepath.Join(tor.StateDir, "services", "dpl_private"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{strings.Repeat("a", 52), testPubkey[:51] + "b"} {
		if err := tor.SetOnionClients(context.Background(), "dpl_private", map[string]string{"client": key}); err == nil {
			t.Fatal("unsafe key accepted")
		}
	}
}

func TestOnionProfileReloadFailureRestoresPolicy(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionProfile(context.Background(), "dpl_private", "max"); err != nil {
		t.Fatal(err)
	}
	tor.reloadFn = func(context.Context) error { return fmt.Errorf("injected reload failure") }
	if err := tor.SetOnionProfile(context.Background(), "dpl_private", "standard"); err == nil {
		t.Fatal("failed downgrade accepted")
	}
	config, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil || !strings.Contains(string(config), "HiddenServicePoWDefensesEnabled 1") {
		t.Fatalf("maximum policy lost: %v", err)
	}
}

func TestOnionAuthRefusesSymlinkDirectory(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "authorized_clients")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := tor.SetOnionClients(context.Background(), "dpl_private", map[string]string{"alice": testPubkey}); err == nil {
		t.Fatal("symlink followed")
	}
	files, err := os.ReadDir(outside)
	if err != nil || len(files) != 0 {
		t.Fatal("wrote outside service")
	}
}

func TestCorruptProfileDoesNotDowngradeTorrc(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionProfile(context.Background(), "dpl_private", "max"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []string{"{", `{"profile":"unexpected"}`} {
		if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte(corrupt), 0600); err != nil {
			t.Fatal(err)
		}
		if err := tor.RegenerateTorrc(); err == nil {
			t.Fatal("corrupt policy silently accepted")
		}
		after, err := os.ReadFile(filepath.Join(tor.StateDir, "torrc"))
		if err != nil || string(after) != string(before) {
			t.Fatal("active configuration changed after corrupt profile")
		}
	}
}

func TestProfileRenderFailureRestoresMetadata(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionProfile(context.Background(), "dpl_private", "max"); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(tor.StateDir, "services", "dpl_broken")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "profile.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionProfile(context.Background(), "dpl_private", "standard"); err == nil {
		t.Fatal("invalid neighboring profile accepted")
	}
	p, err := tor.readProfile("dpl_private")
	if err != nil || p != ProfileMax {
		t.Fatalf("failed render changed stored policy: %s %v", p, err)
	}
}

func TestOnionRemovalFailureAndRepeatedParking(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	put := func(value string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "hs_ed25519_secret_key"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put("first")
	tor.reloadFn = func(context.Context) error { return fmt.Errorf("injected reload failure") }
	if err := tor.RemoveHiddenService(context.Background(), "dpl_private"); err == nil {
		t.Fatal("failed reload reported successful removal")
	}
	tor.reloadFn = func(context.Context) error { return nil }
	if err := tor.RemoveHiddenService(context.Background(), "dpl_private"); err != nil {
		t.Fatal(err)
	}
	put("second")
	if err := tor.RemoveHiddenService(context.Background(), "dpl_private"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(tor.StateDir, "parked"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("repeated parking destroyed recovery copy: %v %v", entries, err)
	}
	old, err := os.ReadFile(filepath.Join(tor.StateDir, "parked", "dpl_private", "hs_ed25519_secret_key"))
	if err != nil || string(old) != "first" {
		t.Fatal("original parked identity overwritten")
	}
}
