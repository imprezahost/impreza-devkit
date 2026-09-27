//go:build linux

package proxy

import (
	"os"

	"golang.org/x/sys/unix"
)

// chmodDirNoFollow applies mode to a directory through a descriptor opened
// with O_NOFOLLOW|O_DIRECTORY: a symlink (or a non-directory) at the final
// component fails the open instead of being followed. A compromised Tor
// container with a writable services/ mount could otherwise swap a service
// directory for a symlink and have the agent — root — tighten the mode of
// an arbitrary host directory.
func chmodDirNoFollow(path string, mode os.FileMode) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Fchmod(fd, uint32(mode.Perm()))
}
