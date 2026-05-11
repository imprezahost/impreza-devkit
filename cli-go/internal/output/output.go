package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"gopkg.in/yaml.v3"
)

// Format is the resolved output mode for a single invocation. Resource
// commands receive a Format value from the root and dispatch to the
// matching renderer.
type Format int

const (
	FormatTable Format = iota
	FormatJSON
	FormatYAML
)

// ParseFormat maps the `--output` flag string to a Format. Unknown
// values produce an error so the root command can surface a clear
// "invalid --output value" message rather than silently falling back.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "table":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	default:
		return FormatTable, fmt.Errorf("unsupported --output %q (expected: table | json | yaml)", s)
	}
}

// String returns the canonical lower-case name of the format. Useful
// for error messages and tests.
func (f Format) String() string {
	switch f {
	case FormatJSON:
		return "json"
	case FormatYAML:
		return "yaml"
	default:
		return "table"
	}
}

// RenderJSON writes v as a 2-space-indented JSON document followed by
// a trailing newline. Resource commands call this when Format == JSON.
func RenderJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// RenderYAML writes v as YAML 1.2 with 2-space indent. Resource
// commands call this when Format == YAML.
//
// To make YAML output respect the `json:"..."` struct tags (matching
// the JSON output exactly), we round-trip through JSON first — marshal
// v to JSON bytes, unmarshal into a generic interface{}, then YAML-
// encode. This avoids having to maintain parallel `yaml:"..."` tags
// alongside `json:"..."` on every struct (and prevents the two from
// drifting). The cost is one extra encode/decode pass; negligible
// for CLI output sizes.
func RenderYAML(w io.Writer, v any) error {
	jsonBytes, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("yaml-via-json marshal: %w", err)
	}
	var intermediate any
	if err := json.Unmarshal(jsonBytes, &intermediate); err != nil {
		return fmt.Errorf("yaml-via-json unmarshal: %w", err)
	}

	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(intermediate); err != nil {
		return err
	}
	return enc.Close()
}

// Table is a thin convenience wrapper around go-pretty's table.Writer
// pre-configured with the project's preferred style (StyleLight,
// no auto-index, no borders). Resource commands construct one,
// AppendHeader + AppendRow, then call Render() with the writer they
// got from cobra.
//
// Usage:
//
//	t := output.NewTable(out)
//	t.AppendHeader(table.Row{"id", "name", "status"})
//	for _, s := range services {
//	    t.AppendRow(table.Row{s.ID, s.ProductName, s.Status})
//	}
//	t.Render()
func NewTable(w io.Writer) table.Writer {
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	// Borders off — looks nicer in CLI use, closer to the Python CLI's
	// `rich.table.Table` defaults.
	style := t.Style()
	style.Options.DrawBorder = false
	style.Options.SeparateRows = false
	style.Options.SeparateColumns = false
	style.Format.Header = text.FormatLower
	return t
}
