//go:build linux

package ingress

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockState serializes the agent, the boot unit and the local reset command
// on the same state directory.
func lockState(stateDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(stateDir, ".ingress.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("ingress state lock unavailable")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, errors.New("ingress state lock unavailable")
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
