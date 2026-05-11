// Package config persists CLI contexts (named API credentials) to a
// TOML file under the OS-appropriate config directory.
//
// Path policy matches the Python CLI:
//
//	Linux:    $XDG_CONFIG_HOME/impreza/config.toml
//	          (default $HOME/.config/impreza/config.toml)
//	macOS:    $HOME/Library/Application Support/impreza/config.toml
//	Windows:  %APPDATA%\impreza\config.toml
//
// Override via IMPREZA_CONFIG=/path/to/config.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ConfigPath returns the absolute path the CLI reads + writes its config
// at. Respects IMPREZA_CONFIG if set; otherwise computes the OS-default
// per the policy above.
func ConfigPath() (string, error) {
	if override := os.Getenv("IMPREZA_CONFIG"); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve IMPREZA_CONFIG: %w", err)
		}
		return abs, nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "impreza", "config.toml"), nil
}

// configDir returns the OS-appropriate base config directory (without
// the trailing "impreza/" segment, which ConfigPath appends).
func configDir() (string, error) {
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd", "netbsd":
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return xdg, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		return filepath.Join(home, ".config"), nil

	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support"), nil

	case "windows":
		// %APPDATA% always points at the Roaming AppData dir on Windows.
		// os.UserConfigDir handles this correctly; using it keeps us
		// future-proof if MS adds OneDrive-redirected variants.
		appdata, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("locate APPDATA: %w", err)
		}
		return appdata, nil

	default:
		// Fall back to the Go stdlib's best guess for anything else
		// (Plan 9, AIX, etc.). Worst case it returns ~/.config.
		return os.UserConfigDir()
	}
}
