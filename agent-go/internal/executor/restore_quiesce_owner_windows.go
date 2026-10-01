//go:build windows

package executor

import "os"

// quiescePreserveOwner is a no-op on Windows: the development double has
// no ownership to preserve, and the agent's production runtime is Linux.
func quiescePreserveOwner(path string, info os.FileInfo) {}
