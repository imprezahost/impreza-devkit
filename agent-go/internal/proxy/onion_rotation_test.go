package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rotationFixture(t *testing.T) (*Tor, *Caddy, string, string) {
	t.Helper()
	tor := newTestTor(t)
	id := "dpl_rotate"
	secret, public, address, err := freshOnionKeyFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tor.ImportOnionKey(id, secret, public); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionClients(context.Background(), id, map[string]string{"alice": testPubkey}); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionProfile(context.Background(), id, "max"); err != nil {
		t.Fatal(err)
	}
	c := New(t.TempDir(), tor.Log)
	c.switchReload = func(context.Context) error { return nil }
	if err := c.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	raw := renderFragment(id, []Route{{Hostname: "example.test", OnionAddr: address, Upstream: "app:80"}})
	if err := os.WriteFile(filepath.Join(c.StateDir, "deployments", id+".caddy"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return tor, c, id, address
}

func TestOnionRotationPreservesPolicyRoutesAndReplay(t *testing.T) {
	tor, c, id, old := rotationFixture(t)
	next, err := tor.RotateOnionIdentity(context.Background(), c, id, "cmd_first", old)
	if err != nil {
		t.Fatal(err)
	}
	if next == old {
		t.Fatal("identity did not rotate")
	}
	names, err := tor.OnionClientNames(id)
	if err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatal("authorization lost")
	}
	if tor.profileOf(id) != "max" {
		t.Fatal("profile lost")
	}
	raw, err := os.ReadFile(filepath.Join(c.StateDir, "deployments", id+".caddy"))
	if err != nil || strings.Contains(string(raw), old) || !strings.Contains(string(raw), next) {
		t.Fatal("routing not replaced")
	}
	if _, err := os.Stat(filepath.Join(tor.StateDir, "parked", id+"-cmd_first", "hs_ed25519_secret_key")); err != nil {
		t.Fatal("old identity not retained")
	}
	replay, err := tor.RotateOnionIdentity(context.Background(), c, id, "cmd_first", old)
	if err != nil || replay != next {
		t.Fatal("command replay rotated again")
	}
	if _, err := tor.RotateOnionIdentity(context.Background(), c, id, "cmd_second", old); err == nil {
		t.Fatal("stale confirmation accepted")
	}
}

func TestOnionRotationReloadFailureRecoversBothLayers(t *testing.T) {
	tor, c, id, old := rotationFixture(t)
	n := 0
	tor.reloadFn = func(context.Context) error {
		n++
		if n == 1 {
			return fmt.Errorf("injected failed Tor reload")
		}
		return nil
	}
	if _, err := tor.RotateOnionIdentity(context.Background(), c, id, "cmd_failure", old); err == nil {
		t.Fatal("failed rotation accepted")
	}
	pub, err := os.ReadFile(filepath.Join(tor.StateDir, "services", id, "hs_ed25519_public_key"))
	if err != nil {
		t.Fatal(err)
	}
	_, address, err := ParsePublicKeyFile(pub)
	if err != nil || address != old {
		t.Fatal("identity not recovered")
	}
	raw, err := os.ReadFile(filepath.Join(c.StateDir, "deployments", id+".caddy"))
	if err != nil || !strings.Contains(string(raw), old) {
		t.Fatal("routing not recovered")
	}
	if err := tor.guardRotation(); err != nil {
		t.Fatal("successful rollback kept journal")
	}
	if tor.profileOf(id) != "max" {
		t.Fatal("rollback lost policy")
	}
}

func TestOnionRotationInterruptedRecoveryBlocksMutations(t *testing.T) {
	tor, c, id, old := rotationFixture(t)
	tor.reloadFn = func(context.Context) error { return fmt.Errorf("daemon unavailable") }
	if _, err := tor.RotateOnionIdentity(context.Background(), c, id, "cmd_crash", old); err == nil {
		t.Fatal("unavailable daemon accepted")
	}
	if err := tor.SetOnionClients(context.Background(), id, nil); err == nil {
		t.Fatal("mutation bypassed recovery")
	}
	tor.reloadFn = func(context.Context) error { return nil }
	if err := tor.RecoverOnionRotation(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := tor.guardRotation(); err != nil {
		t.Fatal(err)
	}
	names, err := tor.OnionClientNames(id)
	if err != nil || len(names) != 1 {
		t.Fatal("recovery lost restricted discovery")
	}
}
