package cmd

import (
	"fmt"
	"io"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/client"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/config"
	"github.com/imprezahost/impreza-devkit/cli-go/internal/output"
)

// newClient is the shared "load config + resolve context + build
// HTTP client" path used by every resource command. Keeps the
// command bodies focused on their resource semantics.
//
// Returns the *client.Client and the resolved context name (useful
// for error messages and the doctor command).
func newClient() (*client.Client, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", err
	}
	name, ctx, err := cfg.Active(globalContext)
	if err != nil {
		return nil, "", err
	}
	c, err := client.New(ctx)
	if err != nil {
		return nil, "", err
	}
	return c, name, nil
}

// resolveFormat parses the global --output flag (or per-command override
// when commands add their own --output flag in 7.5+ polish). For 7.2
// every read command uses the global flag.
func resolveFormat() (output.Format, error) {
	f, err := output.ParseFormat(globalOutput)
	if err != nil {
		return f, fmt.Errorf("--output: %w", err)
	}
	return f, nil
}

// renderJSONOrYAML is the common "machine-readable" branch every
// resource command uses. Table rendering is per-resource (each command
// has its own column spec).
func renderJSONOrYAML(w io.Writer, v any, f output.Format) error {
	switch f {
	case output.FormatJSON:
		return output.RenderJSON(w, v)
	case output.FormatYAML:
		return output.RenderYAML(w, v)
	default:
		// Caller should have branched on format BEFORE calling this.
		return fmt.Errorf("renderJSONOrYAML called with format=%s", f)
	}
}
