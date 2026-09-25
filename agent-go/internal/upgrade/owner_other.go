//go:build !linux

package upgrade

import "os"

// The managed updater is Linux-only; this permits portable unit tests.
func rootOwned(info os.FileInfo) bool { return true }
