//go:build !windows

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

// No symlink following or blocking on FIFOs, including a replacement race.
func openInput(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
