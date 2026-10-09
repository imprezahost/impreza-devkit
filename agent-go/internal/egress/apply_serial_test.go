package egress

// The serialization lives in THIS package. The reconcile ticker and
// the executor's create-a-network reapply both call Apply/Apply6; two
// iptables-restore series on the same chain can drop each other's rules
// (last writer wins per chain). These tests prove the entry points hold
// the package lock — a caller that already holds it blocks them.

import (
	"context"
	"testing"
	"time"
)

func blocksWhileHeld(t *testing.T, name string, call func(ctx context.Context, stateDir string) error) {
	t.Helper()
	applyMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Apply on an empty state dir fails fast on a host without
		// iptables, but it must not even ENTER until the lock is free.
		_ = call(context.Background(), t.TempDir())
	}()
	select {
	case <-done:
		applyMu.Unlock()
		t.Fatalf("%s ran while the package lock was held", name)
	case <-time.After(150 * time.Millisecond):
		// still blocked — the expected serialization
	}
	applyMu.Unlock()
	<-done
}

func TestApplyHoldsThePackageLock(t *testing.T) {
	blocksWhileHeld(t, "Apply", Apply)
}

func TestApply6HoldsThePackageLock(t *testing.T) {
	blocksWhileHeld(t, "Apply6", Apply6)
}
