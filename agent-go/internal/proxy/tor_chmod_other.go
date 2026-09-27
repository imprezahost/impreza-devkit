//go:build !linux

package proxy

import (
	"fmt"
	"os"
)

// chmodDirNoFollow is the portable form for non-Linux development hosts:
// Tor only runs on Linux, so the descriptor-based hardening lives in the
// Linux file; here an Lstat check keeps the refusal behavior (never chmod
// through a symlink) without platform-specific calls.
func chmodDirNoFollow(path string, mode os.FileMode) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe Tor directory")
	}
	return os.Chmod(path, mode)
}
