package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOnionPolicyInterruptedUpdateRecoversBeforeNewMutation(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tor.SetOnionClients(ctx, "dpl_private", map[string]string{"alice": testPubkey}); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(dir, "authorized_clients")
	old, err := os.ReadFile(filepath.Join(auth, "alice.auth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tor.beginPolicyChange(onionPolicyChange{Deployment: "dpl_private", Kind: "auth", Auth: map[string][]byte{"alice.auth": old}}); err != nil {
		t.Fatal(err)
	}
	// Simulate process death between installing the addition and finishing the
	// old-list removal. A fresh manager must recover the journal, not its RAM.
	if err := os.WriteFile(filepath.Join(auth, "bob.auth"), old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(auth, "alice.auth")); err != nil {
		t.Fatal(err)
	}
	restarted := &Tor{StateDir: tor.StateDir, Log: tor.Log, reloadFn: func(context.Context) error { return nil }}
	if err := restarted.SetOnionProfile(ctx, "dpl_private", "max"); err == nil {
		t.Fatal("pending authorization recovery allowed mutation")
	}
	if err := restarted.RecoverOnionPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	names, err := restarted.OnionClientNames("dpl_private")
	if err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatalf("previous authorization not recovered: %v %v", names, err)
	}
	if _, err := os.Stat(restarted.policyPath()); !os.IsNotExist(err) {
		t.Fatal("journal not cleared after successful recovery")
	}
	if err := restarted.SetOnionProfile(ctx, "dpl_private", "max"); err != nil {
		t.Fatal(err)
	}
}

func TestOnionPolicyCommittedReceiptDoesNotUndoAppliedPolicy(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionProfile(context.Background(), "dpl_private", "max"); err != nil {
		t.Fatal(err)
	}
	change := onionPolicyChange{Version: 1, Deployment: "dpl_private", Kind: "profile", Committed: true, HadProfile: true, Profile: []byte(`{"profile":"standard"}`)}
	data, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	if err := torPrivateWrite(tor.policyPath(), data); err != nil {
		t.Fatal(err)
	}
	if err := tor.RecoverOnionPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual, err := tor.readProfile("dpl_private")
	if err != nil || actual != ProfileMax {
		t.Fatal("committed policy was rolled back")
	}
}

func TestOnionPolicyInterruptedProfileRestoresPreviousTier(t *testing.T) {
	tor := newTestTor(t)
	dir := filepath.Join(tor.StateDir, "services", "dpl_private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tor.SetOnionProfile(context.Background(), "dpl_private", "max"); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tor.beginPolicyChange(onionPolicyChange{Deployment: "dpl_private", Kind: "profile", HadProfile: true, Profile: old}); err != nil {
		t.Fatal(err)
	}
	if err := torPrivateWrite(filepath.Join(dir, "profile.json"), []byte(`{"profile":"standard"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tor.RecoverOnionPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual, err := tor.readProfile("dpl_private")
	if err != nil || actual != ProfileMax {
		t.Fatal("previous tier not recovered")
	}
}

func TestCorruptOnionPolicyJournalRemainsBlocked(t *testing.T) {
	tor := newTestTor(t)
	if err := torPrivateWrite(tor.policyPath(), []byte(`{"version":1,"deployment":"../outside","kind":"auth"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tor.RecoverOnionPolicy(context.Background()); err == nil {
		t.Fatal("invalid recovery context accepted")
	}
	if err := tor.guardRotation(); err == nil {
		t.Fatal("corrupt journal silently discarded")
	}
}
