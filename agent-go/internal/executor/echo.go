// Package executor runs the agent's incoming commands. The MVP ships
// a single executor — `Echo` — that pretends to succeed at whatever
// command kind it is handed. Real executors (Docker, systemd, Caddy)
// land in Phase 9.2+.
//
// Every executor implements the small `Executor` interface so the poll
// loop is decoupled from the underlying mechanism.
package executor

import (
	"context"
	"fmt"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Executor handles one PollCommand and returns the DeployResult that
// the agent will report back to the control plane. Implementations
// must be safe to call concurrently — the poll loop only calls
// Execute serially today, but a future scheduler may run several at
// once.
//
// Execute MUST respect ctx cancellation. If ctx is done before the
// command completes, return a DeployResult with Status="timeout" and
// Error set to the cancellation reason.
type Executor interface {
	Execute(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult
}

// Echo is the MVP executor. It always reports success, with a logs_tail
// that names the command kind so the panel can confirm the round-trip
// works end-to-end. Useful for verifying the long-poll, the auth
// realm, and the deploy-result path in isolation before adding any
// Docker / systemd / Caddy machinery.
type Echo struct{}

// Execute satisfies Executor. Value receiver because Echo has no state
// — letting callers compose with `Echo{}.Execute(...)` as a fallback
// inside other executors.
func (e Echo) Execute(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	now := time.Now().UTC().Format(time.RFC3339)
	return sdkclient.DeployResult{
		CommandID: cmd.ID,
		Status:    "success",
		LogsTail: fmt.Sprintf(
			"[%s] echo executor — kind=%s id=%s\nNo real work performed; this is the MVP echo executor.\n",
			now, cmd.Kind, cmd.ID,
		),
	}
}
