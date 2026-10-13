//go:build linux

package executor

// The chown pass races a running container. The writer swaps
// ./data/d between a real directory and a link to a host directory; a path
// walk lists d and then calls lchown("data/d/<name>"), which still resolves
// d. The pass must never change a file outside the tree.

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestDataDirChownRacingLinkSwap(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	base := t.TempDir()
	host := filepath.Join(base, "host-secret")
	if err := os.MkdirAll(host, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(host, "shadow")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "app", "data")
	d := filepath.Join(data, "d")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	// The real directory holds an entry named like the host file.
	if err := os.WriteFile(filepath.Join(d, "shadow"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	swapped := make(chan struct{})
	// The writer must be gone before the test returns (TempDir cleanup).
	defer func() { stop.Store(true); <-swapped }()
	go func() {
		defer close(swapped) // the container: directory <-> link, nonstop
		real := filepath.Join(data, ".d-real")
		for !stop.Load() {
			_ = os.Rename(d, real)
			_ = os.Symlink(host, d)
			time.Sleep(time.Microsecond)
			_ = os.Remove(d)
			_ = os.Rename(real, d)
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	rounds := 0
	for time.Now().Before(deadline) {
		rounds++
		_ = applyDataDirConfig(data, &sdkclient.DataDirConfig{Owner: "1000:1000"})
		var st syscall.Stat_t
		if err := syscall.Stat(target, &st); err == nil && st.Uid == 1000 {
			t.Fatalf("RACE WON after %d rounds: host file %s is now %d:%d", rounds, target, st.Uid, st.Gid)
		}
	}
	t.Logf("no race in %d rounds (20 s)", rounds)
}

// Bind preparation races a running container. The writer swaps
// ./data/a between a real directory and a link to a host directory; a path
// Mkdir("data/a/b") resolves a. Nothing may be created outside the tree.
func TestPrepareDataBindsRacingLinkSwap(t *testing.T) {
	base := t.TempDir()
	host := filepath.Join(base, "host-dir")
	if err := os.MkdirAll(host, 0o700); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "app", "data")
	a := filepath.Join(data, "a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	swapped := make(chan struct{})
	// The writer must be gone before the test returns (TempDir cleanup).
	defer func() { stop.Store(true); <-swapped }()
	go func() {
		defer close(swapped)
		real := filepath.Join(data, ".a-real")
		for !stop.Load() {
			_ = os.Rename(a, real)
			_ = os.Symlink(host, a)
			time.Sleep(time.Microsecond)
			_ = os.Remove(a)
			_ = os.Rename(real, a)
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	rounds := 0
	for time.Now().Before(deadline) {
		rounds++
		_ = os.Remove(filepath.Join(data, ".a-real", "b"))
		_ = prepareDataBinds(data, []string{filepath.Join(data, "a", "b")})
		if _, err := os.Lstat(filepath.Join(host, "b")); err == nil {
			t.Fatalf("RACE WON after %d rounds: created %s outside the data tree", rounds, filepath.Join(host, "b"))
		}
		_ = os.Remove(filepath.Join(a, "b"))
	}
	t.Logf("no race in %d rounds", rounds)
}
