// Package output renders command results across the three formats the
// CLI supports (table, json, yaml) and provides the colour palette
// helpers that mirror the Python CLI (`success`, `info`, `warning`,
// `error`).
//
// Phase 7.1 ships the palette helpers + format-flag plumbing; the table
// renderer + per-resource columns are filled in by the resource commands
// landing in 7.2+ (each calls into `RenderTable`, `RenderJSON`, or
// `RenderYAML` with its own column spec).
package output

import (
	"fmt"
	"io"
	"os"

	"github.com/fatih/color"
)

// NoColor forces colour off across every palette helper. Set by
// `--no-color` on the root command, or auto-detected when stdout is
// not a TTY.
var NoColor = false

// Success writes a green "✓ <msg>" line to stdout. Matches the Python
// CLI's success() helper used for "X created / updated / deleted".
func Success(format string, args ...any) {
	writeColored(os.Stdout, color.FgGreen, "✓ ", format, args...)
}

// Info writes a cyan "ℹ <msg>" line to stdout. Matches Python info() —
// for "queued / reboot to apply / state-change pending" messages.
func Info(format string, args ...any) {
	writeColored(os.Stdout, color.FgCyan, "ℹ ", format, args...)
}

// Warning writes a yellow "⚠ <msg>" line to stderr. Matches Python
// warning() — for "surprising-but-OK" cases.
func Warning(format string, args ...any) {
	writeColored(os.Stderr, color.FgYellow, "⚠ ", format, args...)
}

// Error writes a red "✗ <msg>" line to stderr. Used by every command's
// RunE return path through Cobra's error handling.
func Error(format string, args ...any) {
	writeColored(os.Stderr, color.FgRed, "✗ ", format, args...)
}

func writeColored(w io.Writer, c color.Attribute, prefix, format string, args ...any) {
	if NoColor || !isatty(w) {
		fmt.Fprint(w, prefix)
		fmt.Fprintf(w, format, args...)
		fmt.Fprintln(w)
		return
	}
	col := color.New(c)
	_, _ = col.Fprint(w, prefix)
	_, _ = col.Fprintf(w, format, args...)
	fmt.Fprintln(w)
}

// isatty is a small helper — returns true if w is os.Stdout/Stderr AND
// the underlying file descriptor is a terminal. The `color` package
// already short-circuits on non-TTY when its global `NoColor` is set;
// we keep our own check explicit so callers can reason about it without
// reaching into fatih/color internals.
func isatty(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
