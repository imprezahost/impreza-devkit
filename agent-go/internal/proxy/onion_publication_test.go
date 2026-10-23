package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func publicationFixture(t *testing.T) (*Tor, string, string) {
	t.Helper()
	tor := newTestTor(t)
	id, onion := "dpl_publication", strings.Repeat("a", 56)+".onion"
	if err := os.MkdirAll(filepath.Join(tor.StateDir, "services", id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tor.StateDir, "services", id, "hostname"), []byte(onion+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return tor, id, onion
}

func TestOnionPublicationRefusesSymlinkAndOversize(t *testing.T) {
	tor, id, onion := publicationFixture(t)
	path := filepath.Join(tor.StateDir, "services", id, "hostname")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 129)), 0600); err != nil {
		t.Fatal(err)
	}
	if tor.ownsPublicationTarget(id, onion) {
		t.Fatal("oversize hostname accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tor.StateDir, "missing"), path); err != nil {
		t.Skip("symlink unavailable")
	}
	if tor.ownsPublicationTarget(id, onion) {
		t.Fatal("symlink accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(onion+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(tor.StateDir, "services", id, "hostname-alias")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if tor.ownsPublicationTarget(id, onion) {
		t.Fatal("hardlink accepted")
	}
}

func TestOnionPublicationMissingControllerIsFastWithoutTools(t *testing.T) {
	tor, id, onion := publicationFixture(t)
	t.Setenv("PATH", t.TempDir()) // Neither image requires curl, nc, bash or Docker CLI here.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if tor.WaitForOnionPublication(ctx, id, onion) {
		t.Fatal("hostname alone confirmed")
	}
	if time.Since(start) >= 500*time.Millisecond {
		t.Fatal("unsupported controller waited instead of failing fast")
	}
	if tor.ownsPublicationTarget(id, strings.Repeat("b", 56)+".onion") || tor.ownsPublicationTarget("../escape", onion) {
		t.Fatal("foreign target accepted")
	}
}

func TestOnionPublicationCancelledIsImmediate(t *testing.T) {
	tor, id, onion := publicationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if tor.WaitForOnionPublication(ctx, id, onion) {
		t.Fatal("cancelled observation confirmed")
	}
}
