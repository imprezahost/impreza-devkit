package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/egress"
)

// With the `br+` wildcard gone, the egress chains cover the
// Docker bridges enumerated at the last apply. A network the executor
// creates AFTER that — the shared proxy network, or the deployment's own
// compose network at stack up — carries no per-interface rules until the
// periodic reconcile (up to a minute without the SMTP/rate drops and the
// host chain). The executor reapplies the egress (v4 FORWARD, v6 FORWARD
// and both host halves — Apply and Apply6 carry them) immediately after
// creating a network, and FAILS CLOSED: a failed reapply fails the
// command instead of leaving the new bridge open in silence.
//
// EgressApply is the seam: tests drive it without iptables; production
// leaves it nil and the real egress packages run.
type egressReapplyStage struct{}

func (d *Docker) reapplyEgress(ctx context.Context, stage string) error {
	apply := d.EgressApply
	if apply == nil {
		apply = func(ctx context.Context, stateDir string) error {
			return errors.Join(egress.Apply(ctx, stateDir), egress.Apply6(ctx, stateDir))
		}
	}
	// Fail closed only where the v4 FORWARD baseline was in force. Where it
	// never applies (Docker with "iptables": false, no iptables at all) the
	// agent's contract stays fail-open, as at boot and in the reconcile
	// ticker: failing every deploy there protects nothing.
	enforced := egress.Enforced(d.StateDir)
	if err := apply(context.WithValue(ctx, egressReapplyStage{}, stage), d.StateDir); err != nil {
		if !enforced {
			if d.Log != nil {
				d.Log.Warn("egress reapply failed on a host where the egress baseline is not in force; continuing (fail-open, as at boot)",
					"stage", stage, "err", err)
			}
			return nil
		}
		return fmt.Errorf("%s: egress reapply failed (failing closed): %w", stage, err)
	}
	return nil
}
