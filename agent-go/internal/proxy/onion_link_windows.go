//go:build windows

package proxy

import "os"

// The publication owner check is a Linux inode count. Windows refuses it.
func singleRegularLink(os.FileInfo) bool {
	return false
}
