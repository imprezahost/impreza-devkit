// Package config loads and persists the agent's credentials and
// runtime knobs. Unlike the CLI (multi-context, designed for humans),
// the agent has exactly one identity per host — the credentials issued
// at bootstrap.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

// DefaultPath returns the canonical config location for the current OS.
//
//   - Linux: /etc/impreza-agent/config.toml (system-wide, expected when
//     running as the systemd-managed service).
//   - Other OSes: ~/.config/impreza-agent/config.toml (dev convenience).
//
// The IMPREZA_AGENT_CONFIG environment variable overrides everything.
func DefaultPath() string {
	if env := os.Getenv("IMPREZA_AGENT_CONFIG"); env != "" {
		return env
	}
	if runtime.GOOS == "linux" {
		return "/etc/impreza-agent/config.toml"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".config", "impreza-agent", "config.toml")
}

// Config is the on-disk shape persisted at DefaultPath().
type Config struct {
	// Identity — written by bootstrap, read by run / doctor.
	AgentID         string `toml:"agent_id"`
	AgentSecret     string `toml:"agent_secret"`
	ControlPlaneURL string `toml:"control_plane_url"`

	// Network.
	UseTor bool   `toml:"use_tor,omitempty"`
	Proxy  string `toml:"proxy,omitempty"`

	// Loop tunables.
	BackoffMinSeconds int `toml:"backoff_min_seconds,omitempty"`
	BackoffMaxSeconds int `toml:"backoff_max_seconds,omitempty"`
	HeartbeatSeconds  int `toml:"heartbeat_seconds,omitempty"`
}

// Sentinel errors callers may want to distinguish.
var (
	ErrNoConfig         = errors.New("config file not found — run `impreza-agent bootstrap` first")
	ErrMissingIdentity  = errors.New("config file missing agent_id or agent_secret")
)

// Load reads the config file at DefaultPath() (or the override). Returns
// ErrNoConfig when the file doesn't exist so callers can show a useful
// first-run message rather than a generic "no such file" stat error.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoConfig
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// Save writes the config to the given path (or DefaultPath() when
// empty). Parent directories are created at 0700; the file itself is
// 0600 because it contains a long-lived secret. On Windows the OS
// default ACL is left in place.
func (c *Config) Save(path string) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open config %s for write: %w", path, err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("chmod config: %w", err)
		}
	}
	return nil
}

// Validate checks that the credentials needed for run / doctor are
// present. Returns ErrMissingIdentity when either is empty — usually a
// bootstrap-not-run state, but worth a specific error so the caller
// can tell the operator what to do.
func (c *Config) Validate() error {
	if c.AgentID == "" || c.AgentSecret == "" {
		return ErrMissingIdentity
	}
	return nil
}

// applyDefaults backfills sensible defaults for any field left at its
// zero value. Called automatically from Load.
func (c *Config) applyDefaults() {
	if c.ControlPlaneURL == "" {
		c.ControlPlaneURL = "https://api.imprezahost.com"
	}
	if c.BackoffMinSeconds == 0 {
		c.BackoffMinSeconds = 1
	}
	if c.BackoffMaxSeconds == 0 {
		c.BackoffMaxSeconds = 60
	}
	if c.HeartbeatSeconds == 0 {
		c.HeartbeatSeconds = 30
	}
}
