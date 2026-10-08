package executor

// The v2 stability window, as one testable object: it opens at the first
// all-OK sample, re-arms when the gate SEES a restart, and closes only
// after a full window with no new restart. A restart the window has
// re-armed for is absorbed into its own baseline, so a service that
// restarts once early and then stays up becomes ready one window after
// that restart — it does not re-arm on every sample forever.

import "time"

type v2WindowTracker struct {
	// seen is the restart count already absorbed per service; a count
	// above it is a NEW restart and re-arms the window.
	seen map[string]int
	// start is the moment the current window opened.
	start time.Time
}

// observe feeds one sample into the window. baseline is the restart count
// each service had when the gate began (its own history, not this
// window's); a first observation seeds the absorbed counts from it.
func (w *v2WindowTracker) observe(states []containerState, baseline map[string]int, now time.Time) {
	if w.seen == nil {
		w.seen = map[string]int{}
		for name, count := range baseline {
			w.seen[name] = count
		}
	}
	rearmed := false
	for _, s := range states {
		if s.Restarts > w.seen[s.Name] {
			w.seen[s.Name] = s.Restarts
			rearmed = true
		}
	}
	if rearmed || w.start.IsZero() {
		w.start = now
	}
}

// ready reports whether a full window has passed since the window opened.
func (w *v2WindowTracker) ready(now time.Time, window time.Duration) bool {
	return !w.start.IsZero() && now.Sub(w.start) >= window
}

// v2WindowApplies reports whether the clock-based stability window gates
// this stack: under the v2 protocol, whenever some service in the set has
// no healthcheck — mixed or not — its readiness can only be confirmed by
// surviving the window without a restart.
func v2WindowApplies(policy startupPolicy, states []containerState) bool {
	return policy.V2 && !everyServiceHasHealthcheck(states)
}
