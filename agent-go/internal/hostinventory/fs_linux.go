//go:build linux

package hostinventory

import "golang.org/x/sys/unix"

func (local) FS(path string) (map[string]uint64, error) {
	var s unix.Statfs_t
	if e := unix.Statfs(path, &s); e != nil {
		return nil, e
	}
	return map[string]uint64{"bytes_total": s.Blocks * uint64(s.Bsize), "bytes_available": s.Bavail * uint64(s.Bsize), "inodes_total": s.Files, "inodes_available": s.Ffree}, nil
}
