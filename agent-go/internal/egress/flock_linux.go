//go:build linux

package egress

import (
	"os"
	"path/filepath"
	"syscall"
)

// stateDirLock serializes applies ACROSS processes on the same host:
// the daemon and an operator's CLI invocation (each with its own
// in-process mutex) can still interleave iptables-restore series on the
// same chains. An exclusive flock on <stateDir>/egress.apply.lock closes
// that; the in-process applyMu stays — it makes the entry points safe
// even before the file exists.
func stateDirLock(stateDir string) (release func(), err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "egress.apply.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
