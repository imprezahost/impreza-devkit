package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/client"
)

// waitForOperation polls an Operation future and renders a single
// in-place progress line (carriage-return redraw, no scroll). Matches
// the Python CLI's _wait_for_operation behaviour.
//
// On success, prints a final "Operation succeeded" line + returns nil.
// On failure, returns the wrapped ErrOperationFailed; on timeout,
// returns ErrOperationTimeout.
//
// Pass timeoutSeconds=0 to use the default (600s).
func waitForOperation(ctx context.Context, out io.Writer, op *client.Operation, timeoutSeconds int) error {
	if op == nil {
		return nil // synchronous endpoint — nothing to wait on
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeoutSeconds == 0 {
		timeout = 10 * time.Minute
	}

	start := time.Now()
	const padding = 100 // overwrite any leftover characters from a previous redraw

	render := func(op *client.Operation) {
		elapsed := time.Since(start).Round(time.Second)
		line := fmt.Sprintf("Operation %s — state=%s progress=%d%% (elapsed %s)",
			op.UUID[:min(8, len(op.UUID))], op.State, op.Progress, elapsed)
		fmt.Fprintf(out, "\r%-*s", padding, line)
	}

	render(op)
	finalOp, err := op.Wait(ctx, client.WaitOptions{
		Timeout:      timeout,
		PollInterval: 2 * time.Second,
		OnUpdate:     render,
	})

	// Move to a fresh line so subsequent output doesn't overprint.
	fmt.Fprintln(out)

	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Operation succeeded after %s.\n", time.Since(start).Round(time.Second))
	_ = finalOp
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
