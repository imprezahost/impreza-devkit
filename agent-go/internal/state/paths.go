// Package state owns the agent's on-disk paths and (later) the BoltDB
// store that tracks installed apps. For the echo-MVP we only need
// path constants and directory bootstrapping so future executors can
// drop binaries / data without ad-hoc mkdir calls.
package state

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDir returns the canonical state directory for the current OS.
//
//   - Linux: /var/lib/impreza-agent
//   - Other:  ~/.local/state/impreza-agent (dev convenience)
//
// IMPREZA_AGENT_STATE overrides this.
func DefaultDir() string {
	if env := os.Getenv("IMPREZA_AGENT_STATE"); env != "" {
		return env
	}
	if runtime.GOOS == "linux" {
		return "/var/lib/impreza-agent"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "state"
	}
	return filepath.Join(home, ".local", "state", "impreza-agent")
}

// Ensure creates the state directory hierarchy at the given root (or
// DefaultDir() when empty) with 0700 permissions on Unix. Idempotent.
func Ensure(root string) (string, error) {
	if root == "" {
		root = DefaultDir()
	}
	for _, sub := range []string{"", "apps", "tmp"} {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return root, err
		}
	}
	return root, nil
}
