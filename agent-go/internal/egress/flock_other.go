//go:build !linux

package egress

// Cross-process serialization is a Linux-host property (the daemon and
// the CLI that matter run there); on the other build platforms the
// in-process applyMu carries the contract alone.
func stateDirLock(string) (func(), error) { return func() {}, nil }
