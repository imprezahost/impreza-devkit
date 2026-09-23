//go:build linux

package scanner

import (
	"os"
	"syscall"
)

// Refuse symlinks and never wait on a FIFO replaced after directory traversal.
func openNoFollow(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
