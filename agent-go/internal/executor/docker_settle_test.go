package executor

import "testing"

// TestContainerStateOK pins the classification table the post-`up` settle
// gate depends on. The subtle entries are the ones worth guarding:
//
//   - exited/0 is OK, because a stack may include a one-shot migration or
//     init service that does its job and stops. Treating that as a failed
//     deploy would break those stacks.
//   - running with an empty Health is OK, because most catalog images
//     declare no HEALTHCHECK at all; requiring "healthy" would fail
//     almost every app in the catalog.
//   - running/unhealthy is NOT ok, but the caller only ever turns that
//     into an advisory note, never a failed deploy.
func TestContainerStateOK(t *testing.T) {
	cases := []struct {
		name string
		in   containerState
		want bool
	}{
		{"running, no healthcheck declared", containerState{Status: "running"}, true},
		{"running and healthy", containerState{Status: "running", Health: "healthy"}, true},
		{"running but still starting", containerState{Status: "running", Health: "starting"}, false},
		{"running but unhealthy", containerState{Status: "running", Health: "unhealthy"}, false},
		{"one-shot that exited cleanly", containerState{Status: "exited", ExitCode: 0}, true},
		{"exited with a failure code", containerState{Status: "exited", ExitCode: 7}, false},
		{"restarting", containerState{Status: "restarting", ExitCode: 1}, false},
		{"created but never started", containerState{Status: "created"}, false},
		{"paused", containerState{Status: "paused"}, false},
		{"dead", containerState{Status: "dead"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.ok(); got != c.want {
				t.Errorf("ok() = %v, want %v for %+v", got, c.want, c.in)
			}
		})
	}
}

// TestDescribeStates checks the operator-facing summary, since on an
// advisory miss it is the only record of why the app was never confirmed
// healthy.
func TestDescribeStates(t *testing.T) {
	got := describeStates([]containerState{
		{Name: "pg_dpl_1", Status: "restarting", Restarts: 11, ExitCode: 1},
		{Name: "evo_dpl_1", Status: "running", Health: "healthy"},
	})
	want := "pg_dpl_1=restarting restarts=11; evo_dpl_1=running/healthy"
	if got != want {
		t.Errorf("describeStates() =\n  %q\nwant\n  %q", got, want)
	}
}

// TestDescribeStatesEmpty guards against a nil slice panicking, which
// would turn a missing-container edge case into a crashed agent.
func TestDescribeStatesEmpty(t *testing.T) {
	if got := describeStates(nil); got != "" {
		t.Errorf("describeStates(nil) = %q, want empty", got)
	}
}
