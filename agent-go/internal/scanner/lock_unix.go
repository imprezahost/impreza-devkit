//go:build !windows

package scanner

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockAdvisories(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func syncAdvisoryDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
