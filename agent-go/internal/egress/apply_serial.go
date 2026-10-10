package egress

import "sync"

// applyMu serializes the Apply/Apply6 entry points inside the egress
// package. The boot apply, the periodic reconcile ticker, the
// firewalld-reload series and the executor's create-a-network reapply all
// build the SAME chains from the SAME state; interleaving two
// iptables-restore transactions on one chain can drop rules written by
// the other (last writer wins per chain). The lock lives HERE so every
// caller is covered by construction — cmd/run.go's own egressMu was
// removed when this landed, and no future path can forget it.
var applyMu sync.Mutex

// applyEntered, when set, is invoked with the package lock held at the
// top of every apply critical section. It is the deterministic
// seam the lock tests observe: a timing-only proof ("did not finish in
// 150 ms") let the no-lock mutation pass on Windows, where a failing
// apply can legitimately take longer than the window.
var applyEntered func()

// enterApply takes the package lock (and signals the test seam) and
// returns the release. Call as `defer enterApply()()`. With a stateDir it
// also holds the cross-process flock; an empty stateDir skips it
// (the CLI forms that only render rules).
func enterApply(stateDir string) func() {
	applyMu.Lock()
	var releaseFlock func()
	if stateDir != "" {
		if release, err := stateDirLock(stateDir); err == nil {
			releaseFlock = release
		}
		// A lock failure never blocks the apply: the in-process mutex
		// still serializes this process, and the status records what
		// happened to the rules themselves.
	}
	if applyEntered != nil {
		applyEntered()
	}
	return func() {
		if releaseFlock != nil {
			releaseFlock()
		}
		applyMu.Unlock()
	}
}
