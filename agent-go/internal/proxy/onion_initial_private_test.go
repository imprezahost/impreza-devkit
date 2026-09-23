package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitialPrivateOnionNeverInstallsPublicDirectory(t *testing.T) {
	tor := newTestTor(t)
	id := "dpl_initialprivate"
	dir := filepath.Join(tor.StateDir, "services", id)
	for _, clients := range []map[string]string{nil, {}, {"alice": "bad"}, {"../alice": testPubkey}} {
		if err := tor.PrepareInitialPrivate(id, "standard", clients); err == nil {
			t.Fatal("invalid policy accepted")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid policy installed a service")
		}
	}
	clients := map[string]string{"alice": testPubkey, "bob": testPubkey}
	if err := tor.PrepareInitialPrivate(id, "standard", clients); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		if _, err := os.Stat(filepath.Join(dir, "authorized_clients", name+".auth")); err != nil {
			t.Fatal(err)
		}
	}
	// A retry must preserve revocation, not reinstall the original client list.
	if err := os.Remove(filepath.Join(dir, "authorized_clients", "bob.auth")); err != nil {
		t.Fatal(err)
	}
	if err := tor.PrepareInitialPrivate(id, "standard", clients); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "authorized_clients", "bob.auth")); !os.IsNotExist(err) {
		t.Fatal("retry resurrected revoked client")
	}
	if err := tor.PrepareInitialPrivate(id, "standard", map[string]string{"bob": testPubkey}); err == nil {
		t.Fatal("changed initial policy accepted")
	}
	if err := os.Remove(filepath.Join(dir, "authorized_clients", "alice.auth")); err != nil {
		t.Fatal(err)
	}
	if err := tor.PrepareInitialPrivate(id, "standard", clients); err == nil {
		t.Fatal("public state accepted as a private retry")
	}
}

func TestInitialPrivateOnionRefusesExistingPublicService(t *testing.T) {
	tor := newTestTor(t)
	if err := tor.PrepareInitialProfile("dpl_existing", "standard"); err != nil {
		t.Fatal(err)
	}
	if err := tor.PrepareInitialPrivate("dpl_existing", "standard", map[string]string{"alice": testPubkey}); err == nil {
		t.Fatal("public service silently converted")
	}
}
