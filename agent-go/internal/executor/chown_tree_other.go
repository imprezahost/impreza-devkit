//go:build !linux

package executor

import (
	"os"
	"path/filepath"
)

// chownTree outside Linux (development builds only; the agent runs on
// Linux): a path walk with lchown, which does not follow the last component.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return lchown(path, uid, gid)
	})
}
