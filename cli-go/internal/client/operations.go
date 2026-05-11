package client

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OperationState is the terminal status reported by the Proxmox queue.
type OperationState string

const (
	OperationStateQueued    OperationState = "queued"
	OperationStateRunning   OperationState = "running"
	OperationStateSucceeded OperationState = "succeeded"
	OperationStateFailed    OperationState = "failed"
)

// Operation is a long-running Proxmox queue job (reinstall, migrate,
// snapshot rollback, backup create/restore). Returned by the verbs
// that kick those off; call Wait() to block until terminal.
type Operation struct {
	UUID       string         `json:"uuid"`
	State      OperationState `json:"state"`
	Progress   int            `json:"progress,omitempty"`    // 0-100
	Message    string         `json:"message,omitempty"`
	Error      string         `json:"error,omitempty"`
	CreatedAt  string         `json:"created_at,omitempty"`
	UpdatedAt  string         `json:"updated_at,omitempty"`
	FinishedAt string         `json:"finished_at,omitempty"`

	// Service id + client back-reference so .Wait() can poll without
	// the caller re-supplying them. Set by the verb that constructs
	// the Operation (not populated from server JSON).
	serviceID int       `json:"-"`
	client    *Client   `json:"-"`
}

// ErrOperationFailed is returned by Wait() when the server reports
// state="failed". The error message includes the .Error / .Message
// fields from the final poll.
var ErrOperationFailed = errors.New("operation failed")

// ErrOperationTimeout is returned by Wait() when the polling deadline
// elapses before the operation reaches a terminal state.
var ErrOperationTimeout = errors.New("operation timed out")

// IsTerminal returns true if state is succeeded or failed (i.e. no
// further polling needed).
func (op *Operation) IsTerminal() bool {
	return op.State == OperationStateSucceeded || op.State == OperationStateFailed
}

// WaitOptions controls how Wait() behaves. Zero values default to:
//   Timeout=600s (10min), PollInterval=2s, OnUpdate=nil.
type WaitOptions struct {
	Timeout      time.Duration
	PollInterval time.Duration
	// OnUpdate is called after each poll with the latest operation
	// state. Use it to render progress in a CLI.
	OnUpdate func(op *Operation)
}

// Wait polls /vps/proxmox/{service_id}/operations/{uuid} until the
// operation reaches a terminal state, the deadline elapses, or ctx
// is cancelled.
//
// Returns the final Operation snapshot on success.
// Returns ErrOperationFailed (wrapped) if state=="failed".
// Returns ErrOperationTimeout (wrapped) if the deadline hits first.
func (op *Operation) Wait(ctx context.Context, opts WaitOptions) (*Operation, error) {
	if op.client == nil {
		return nil, errors.New("Operation not bound to a Client (constructed without ctx?)")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 600 * time.Second
	}
	interval := opts.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}

	deadline := time.Now().Add(timeout)
	path := fmt.Sprintf("/v1/vps/proxmox/%d/operations/%s", op.serviceID, op.UUID)

	for {
		if op.IsTerminal() {
			if op.State == OperationStateFailed {
				msg := op.Error
				if msg == "" {
					msg = op.Message
				}
				return op, fmt.Errorf("%w: %s", ErrOperationFailed, msg)
			}
			return op, nil
		}

		if time.Now().After(deadline) {
			return op, fmt.Errorf("%w after %s (last state: %s)", ErrOperationTimeout, timeout, op.State)
		}

		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-time.After(interval):
		}

		var next Operation
		if err := op.client.Get(ctx, path, nil, &next); err != nil {
			return op, fmt.Errorf("poll operation: %w", err)
		}
		// Carry forward the bindings — server doesn't send them.
		next.serviceID = op.serviceID
		next.client = op.client
		*op = next

		if opts.OnUpdate != nil {
			opts.OnUpdate(op)
		}
	}
}

// bindOperation attaches the client + service id to an Operation so
// .Wait() can poll. Internal helper used by reinstall/migrate.
func (c *Client) bindOperation(op *Operation, serviceID int) *Operation {
	op.client = c
	op.serviceID = serviceID
	return op
}
