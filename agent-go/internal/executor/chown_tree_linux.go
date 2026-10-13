//go:build linux

package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// maxChownDepth bounds the recursion; a data tree deeper than this is refused.
const maxChownDepth = 256

// chownTree changes the owner of root and of everything under it without
// following a symbolic link at ANY depth. The containers of the previous
// release keep running (and writing ./data) during a redeploy, so a path
// walk is not enough: Lchown("data/d/x") still resolves "d", and a writer
// that swaps the directory d for a link between the listing and the chown
// redirects it to a host file. Here every directory is opened relative to
// its parent's descriptor with O_NOFOLLOW, and every entry is changed with
// fchownat(AT_SYMLINK_NOFOLLOW) on a single name, so a swap fails the open
// or changes the link itself, never its target.
func chownTree(root string, uid, gid int) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", root, err)
	}
	defer unix.Close(fd)
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", root, err)
	}
	return chownDirFD(fd, root, uid, gid, 0)
}

func chownDirFD(dirfd int, path string, uid, gid, depth int) error {
	if depth >= maxChownDepth {
		return fmt.Errorf("%s is nested deeper than %d levels; refusing to continue", path, maxChownDepth)
	}
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dir := os.NewFile(uintptr(dup), path)
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, name := range names {
		child := filepath.Join(path, name)
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // removed by a running container meanwhile
			}
			return fmt.Errorf("inspect %s: %w", child, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := unix.Fchownat(dirfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("chown %s: %w", child, err)
			}
			continue
		}
		cfd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			switch {
			case errors.Is(err, unix.ENOENT):
				continue
			case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
				// Swapped for a link or a file after the stat: change that
				// entry itself, never what it points to.
				if err := unix.Fchownat(dirfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil && !errors.Is(err, unix.ENOENT) {
					return fmt.Errorf("chown %s: %w", child, err)
				}
				continue
			}
			return fmt.Errorf("open %s: %w", child, err)
		}
		err = unix.Fchown(cfd, uid, gid)
		if err == nil {
			err = chownDirFD(cfd, child, uid, gid, depth+1)
		} else {
			err = fmt.Errorf("chown %s: %w", child, err)
		}
		unix.Close(cfd)
		if err != nil {
			return err
		}
	}
	return nil
}
