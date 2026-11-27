package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// confirmOrExit asks the user to confirm a destructive action. Returns
// nil if approved, an error otherwise. Matches the Python CLI's
// `confirm_or_exit` semantics: any answer other than "y" / "yes"
// (case-insensitive) aborts.
//
// If autoYes is true (from --yes / -y), skips the prompt and returns
// immediately.
//
// stdin is read line-by-line from `in`; the prompt is written to
// `out`. Both default to os.Stdin / os.Stdout when the helper is
// called from a Cobra RunE — pass cmd.InOrStdin() / cmd.OutOrStdout()
// to make tests easier.
func confirmOrExit(in io.Reader, out io.Writer, prompt string, autoYes bool) error {
	if autoYes {
		return nil
	}
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stderr
	}
	if _, err := fmt.Fprintf(out, "%s [y/N] ", prompt); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return fmt.Errorf("read confirmation: %w", err)
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "y" || ans == "yes" {
		return nil
	}
	return fmt.Errorf("aborted by user")
}
