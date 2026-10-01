//go:build !windows

package executor

import (
	"os"
	"syscall"
)

// quiescePreserveOwner puts the original uid/gid back on a tree the undo
// copied across filesystems. The restore script extracts with ownership
// intact; the rollback must not hand the application root-owned files.
func quiescePreserveOwner(path string, info os.FileInfo) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(stat.Uid), int(stat.Gid))
	}
}
