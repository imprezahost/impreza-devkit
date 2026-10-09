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
