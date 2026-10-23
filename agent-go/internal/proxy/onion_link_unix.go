//go:build !windows

package proxy

import (
	"os"
	"syscall"
)

// A second name for the same inode could be another deployment's hostname.
// Symlinks are already excluded because they are not regular files.
func singleRegularLink(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
