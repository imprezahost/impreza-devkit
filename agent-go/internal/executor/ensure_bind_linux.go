//go:build linux

package executor

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ensureBindSource creates the missing components of one bind source under
// dataDir. The containers of the previous release may still write ./data
// during a redeploy, so a path Mkdir is not enough: Mkdir("data/a/b") still
// resolves "a", and a writer that swaps a for a link between the check and
// the create puts a root-owned directory outside the tree. Every component
// is created with mkdirat and opened with O_NOFOLLOW relative to its
// parent's descriptor, so a swap is refused instead of followed.
func ensureBindSource(dataDir, src string, parts []string) error {
	fd, err := unix.Open(dataDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", dataDir, err)
	}
	defer func() { unix.Close(fd) }()
	cur := dataDir
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		last := i == len(parts)-1
		if err := unix.Mkdirat(fd, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create bind source %s: %w", cur, err)
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, part, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("inspect bind source %s: %w", cur, err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			return fmt.Errorf("bind source %s goes through a symbolic link; refusing to follow it", cur)
		case unix.S_IFDIR:
		default:
			if last {
				return nil // an existing file bind stays as it is
			}
			return fmt.Errorf("bind source %s: %s is not a directory", src, cur)
		}
		if last {
			return nil
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return fmt.Errorf("bind source %s goes through a symbolic link; refusing to follow it", cur)
			}
			return fmt.Errorf("open bind source %s: %w", cur, err)
		}
		unix.Close(fd)
		fd = next
	}
	return nil
}
