package cmd

// Shared output helpers for human-facing command renders. Honors the
// global --no-color flag + auto-disables ANSI when stdout isn't a TTY
// (CI pipes, logs, json output, etc.) so escape sequences don't leak
// into machine-readable consumers.
//
// Used by `impreza deploy` (Phase 13 polish) for the [step/total]
// progress prefix + colored ✓/⚠/✗ markers. Other commands can adopt
// piecemeal — the helpers are small + don't change the existing
// render path until the caller opts in.

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// ansi escape sequences. Empty when color is disabled.
type colorScheme struct {
	reset  string
	bold   string
	dim    string
	green  string
	yellow string
	red    string
	blue   string
	cyan   string
}

var (
	colorOn = colorScheme{
		reset:  "\x1b[0m",
		bold:   "\x1b[1m",
		dim:    "\x1b[2m",
		green:  "\x1b[32m",
		yellow: "\x1b[33m",
		red:    "\x1b[31m",
		blue:   "\x1b[34m",
		cyan:   "\x1b[36m",
	}
	colorOff = colorScheme{}
)

// activeColors picks colored vs plain based on `globalNoColor` and
// whether stdout is a real TTY. Called per-render so a mid-session
// terminal change is honoured (rare but cheap).
func activeColors(w io.Writer) colorScheme {
	if globalNoColor {
		return colorOff
	}
	f, ok := w.(*os.File)
	if !ok {
		return colorOff
	}
	if !term.IsTerminal(int(f.Fd())) {
		return colorOff
	}
	return colorOn
}

// Stepper renders "[n/total]" prefixes for multi-step human commands.
// Use one Stepper per command invocation. NextStep increments + emits
// the prefix line; Done / Warn / Error emit terminal markers without
// touching the step counter.
type Stepper struct {
	w     io.Writer
	total int
	cur   int
	c     colorScheme
}

// NewStepper returns a Stepper that writes to w + targets `total`
// steps. Pass the cobra command's OutOrStdout() as w so test capture
// stays clean.
func NewStepper(w io.Writer, total int) *Stepper {
	return &Stepper{w: w, total: total, c: activeColors(w)}
}

// Step prints "[n/total] msg" and bumps the counter. Use for the
// HEADER of a step; sub-output goes through plain Fprintf or Info /
// Detail / Done.
func (s *Stepper) Step(msg string) {
	s.cur++
	fmt.Fprintf(s.w, "%s[%d/%d]%s %s%s%s\n",
		s.c.dim, s.cur, s.total, s.c.reset,
		s.c.bold, msg, s.c.reset)
}

// Detail prints "    msg" (indented, dim) — for sub-information lines
// under a Step.
func (s *Stepper) Detail(msg string) {
	fmt.Fprintf(s.w, "    %s%s%s\n", s.c.dim, msg, s.c.reset)
}

// Done prints "    ✓ msg" in green — marks a step's terminal success.
func (s *Stepper) Done(msg string) {
	fmt.Fprintf(s.w, "    %s✓%s %s\n", s.c.green, s.c.reset, msg)
}

// Warn prints "    ⚠ msg" in yellow — non-fatal heads-up.
func (s *Stepper) Warn(msg string) {
	fmt.Fprintf(s.w, "    %s⚠%s %s\n", s.c.yellow, s.c.reset, msg)
}

// Error prints "    ✗ msg" in red — used for terminal failures the
// command is about to return as an error too.
func (s *Stepper) Error(msg string) {
	fmt.Fprintf(s.w, "    %s✗%s %s\n", s.c.red, s.c.reset, msg)
}

// Banner prints a final summary block — used by `impreza deploy` to
// highlight the resolved URL + onion + dpl_id once the polling exits.
// Caller passes title (e.g. "✓ Deployed") + key/value pairs to align.
func (s *Stepper) Banner(title string, kv []KV) {
	fmt.Fprintf(s.w, "\n%s%s%s\n", s.c.bold+s.c.green, title, s.c.reset)
	maxLabel := 0
	for _, p := range kv {
		if len(p.Key) > maxLabel {
			maxLabel = len(p.Key)
		}
	}
	for _, p := range kv {
		fmt.Fprintf(s.w, "  %s%-*s%s  %s\n",
			s.c.dim, maxLabel, p.Key, s.c.reset, p.Value)
	}
	fmt.Fprintln(s.w)
}

// KV is one row in a Stepper.Banner — small struct to keep the call
// site readable.
type KV struct {
	Key   string
	Value string
}
