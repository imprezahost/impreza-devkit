package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

// Context is a single named API credential entry. Multiple contexts
// can live in one config file; one is marked default via Config.Default.
type Context struct {
	Key    string `toml:"key"`              // API key (starts with "imp_")
	Secret string `toml:"secret"`           // API secret (shown once on key creation)
	URL    string `toml:"url,omitempty"`    // override the default https://api.imprezahost.com
	UseTor bool   `toml:"use_tor,omitempty"`
	Proxy  string `toml:"proxy,omitempty"`  // e.g. "socks5://127.0.0.1:9050"
}

// Config is the on-disk shape of $CONFIG/impreza/config.toml.
type Config struct {
	// Default is the context name returned by Active() when no
	// per-invocation override is in effect.
	Default string `toml:"default,omitempty"`

	// Contexts maps a user-chosen name (e.g. "personal", "ci-bot") to
	// the credential entry.
	Contexts map[string]Context `toml:"contexts"`
}

// Sentinel errors for callers that need to distinguish.
var (
	ErrNoConfig          = errors.New("no config file found — run `impreza context create` to set up your first context")
	ErrContextNotFound   = errors.New("context not found")
	ErrNoDefaultContext  = errors.New("no default context set — pass --context NAME or run `impreza context use NAME`")
	ErrDuplicateContext  = errors.New("a context with that name already exists")
)

// Load reads the config file from the OS-default path (or
// IMPREZA_CONFIG override). Returns ErrNoConfig if the file does not
// exist — callers can treat that as "first run" rather than a fatal
// error.
func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	return LoadFromPath(path)
}

// LoadFromPath is the test-friendly form of Load that accepts an
// explicit path argument.
func LoadFromPath(path string) (*Config, error) {
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
	if cfg.Contexts == nil {
		cfg.Contexts = make(map[string]Context)
	}
	return &cfg, nil
}

// Save writes the config to the OS-default path, creating parent
// directories as needed. On POSIX systems the file is chmod 0600 after
// every write so only the owner can read the credentials.
func (c *Config) Save() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return c.SaveToPath(path)
}

// SaveToPath is the test-friendly form of Save.
func (c *Config) SaveToPath(path string) error {
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

	// Re-chmod on POSIX in case the file already existed with looser
	// permissions. Windows ACL handling stays with the OS default.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("chmod config: %w", err)
		}
	}
	return nil
}

// Active returns the context the CLI should use for this invocation.
// Priority:
//
//  1. Per-invocation override (`--context NAME` global flag, passed in
//     as the `override` argument).
//  2. Config.Default, if set.
//
// Returns ErrContextNotFound or ErrNoDefaultContext as appropriate so
// callers can render specific error messages.
func (c *Config) Active(override string) (string, Context, error) {
	name := override
	if name == "" {
		name = c.Default
	}
	if name == "" {
		return "", Context{}, ErrNoDefaultContext
	}
	ctx, ok := c.Contexts[name]
	if !ok {
		return "", Context{}, fmt.Errorf("%w: %s", ErrContextNotFound, name)
	}
	return name, ctx, nil
}

// Add inserts a new context. Returns ErrDuplicateContext if the name
// already exists — callers that want overwrite semantics should
// Delete first or call Update.
func (c *Config) Add(name string, ctx Context) error {
	if c.Contexts == nil {
		c.Contexts = make(map[string]Context)
	}
	if _, exists := c.Contexts[name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateContext, name)
	}
	c.Contexts[name] = ctx
	// First context auto-promotes to default — matches the Python CLI's
	// behavior so `impreza context create personal ...` is enough to
	// start using the CLI.
	if c.Default == "" {
		c.Default = name
	}
	return nil
}

// Delete removes a context. If it was the default, Default is cleared
// (caller must `impreza context use` another one before any network
// command will succeed). Returns ErrContextNotFound if the name is
// not present.
func (c *Config) Delete(name string) error {
	if _, exists := c.Contexts[name]; !exists {
		return fmt.Errorf("%w: %s", ErrContextNotFound, name)
	}
	delete(c.Contexts, name)
	if c.Default == name {
		c.Default = ""
	}
	return nil
}

// SetDefault changes which context is returned by Active() when no
// override is in effect. Returns ErrContextNotFound if the named
// context doesn't exist.
func (c *Config) SetDefault(name string) error {
	if _, exists := c.Contexts[name]; !exists {
		return fmt.Errorf("%w: %s", ErrContextNotFound, name)
	}
	c.Default = name
	return nil
}

// NewEmpty returns an empty Config ready to be populated and saved.
// Used on first-run when Load returns ErrNoConfig.
func NewEmpty() *Config {
	return &Config{
		Contexts: make(map[string]Context),
	}
}
