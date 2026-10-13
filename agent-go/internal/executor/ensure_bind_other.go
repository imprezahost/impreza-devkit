//go:build !linux

package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ensureBindSource outside Linux (development builds only; the agent runs on
// Linux): a component-by-component Lstat and Mkdir.
func ensureBindSource(dataDir, src string, parts []string) error {
	cur := dataDir
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return fmt.Errorf("create bind source %s: %w", cur, err)
			}
		case err != nil:
			return fmt.Errorf("inspect bind source %s: %w", cur, err)
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("bind source %s goes through a symbolic link; refusing to follow it", cur)
		case !fi.IsDir() && i < len(parts)-1:
			return fmt.Errorf("bind source %s: %s is not a directory", src, cur)
		}
	}
	return nil
}
