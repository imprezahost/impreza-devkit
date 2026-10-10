package egress

// The serialization lives in THIS package. The reconcile ticker
// and the executor's create-a-network reapply both call Apply/Apply6; two
// iptables-restore series on the same chain can drop each other's rules
// (last writer wins per chain). The entry seam (applyEntered) makes the
// proof deterministic: a build without the lock ENTERS immediately while
// the lock is held — no timing window to miss (the 150 ms proof let the
// no-lock mutation pass on Windows, where a failing apply is slower than
// the window).

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func entriesWhileHeld(t *testing.T, name string, call func(ctx context.Context, stateDir string) error) {
	t.Helper()
	var entered atomic.Bool
	applyMu.Lock()
	prev := applyEntered
	applyEntered = func() { entered.Store(true) }
	t.Cleanup(func() { applyMu.Lock(); applyEntered = prev; applyMu.Unlock() })
	go func() { _ = call(context.Background(), "") }()
	// While the lock is held the entry must NOT run — checked BEFORE the
	// release, so the goroutine cannot win a race. A no-lock build enters
	// instantly; the short window catches it deterministically.
	time.Sleep(50 * time.Millisecond)
	if entered.Load() {
		applyMu.Unlock()
		t.Fatalf("%s entered while the package lock was held (no-lock mutation caught)", name)
	}
	applyMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for !entered.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never entered after the lock was released", name)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestApplyHoldsThePackageLock(t *testing.T) {
	entriesWhileHeld(t, "Apply", Apply)
}

func TestApply6HoldsThePackageLock(t *testing.T) {
	entriesWhileHeld(t, "Apply6", Apply6)
}

// The cross-process flock on the state dir. Two file descriptors in
// the same process contend for it the same way two processes do.
func TestStateDirFlockExcludesASecondHolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the flock seam is Linux-only")
	}
	dir := t.TempDir()
	release, err := stateDirLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(blocked)
		rel, err := stateDirLock(dir)
		if err != nil {
			t.Error(err)
			return
		}
		close(acquired)
		rel()
	}()
	<-blocked
	select {
	case <-acquired:
		t.Fatal("a second holder acquired the state dir lock while it was held")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the second holder never acquired the lock after the release")
	}
}

// enterApply with a state dir fires the entry seam under the package
// lock and, on Linux, holds the state dir flock for the whole section.
func TestEnterApplySignalsUnderTheLockWithTheFlockHeld(t *testing.T) {
	dir := t.TempDir()
	var fired atomic.Bool
	applyMu.Lock()
	prev := applyEntered
	applyEntered = func() { fired.Store(true) }
	applyMu.Unlock()
	t.Cleanup(func() { applyMu.Lock(); applyEntered = prev; applyMu.Unlock() })
	release := enterApply(dir)
	if !fired.Load() {
		release()
		t.Fatal("the entry seam did not fire")
	}
	if runtime.GOOS == "linux" {
		// The flock is held for the whole section: a direct stateDirLock
		// on the same dir blocks until the release.
		got := make(chan struct{})
		go func() {
			rel, err := stateDirLock(dir)
			if err != nil {
				t.Error(err)
				return
			}
			close(got)
			rel()
		}()
		select {
		case <-got:
			t.Error("the state dir lock was not held by enterApply")
		case <-time.After(100 * time.Millisecond):
		}
		release()
		select {
		case <-got:
		case <-time.After(2 * time.Second):
			t.Fatal("the state dir lock was not released by enterApply's release")
		}
		return
	}
	release()
}
