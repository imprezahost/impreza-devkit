package executor

import "testing"

func TestContainerInspectRequiresCompleteState(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		expected int
	}{
		{"unknown exit", "/init|exited|0|unknown|", 1},
		{"unknown restarts", "/app|running|unknown|0|healthy", 1},
		{"missing rows", "/app|running|0|0|healthy", 2},
		{"malformed row", "/app|running|0|0|healthy\n/init|exited", 2},
		{"duplicate rows", "/app|running|0|0|healthy\n/app|running|0|0|healthy", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if states, err := parseContainerInspect([]byte(tc.out), tc.expected); err == nil || states != nil {
				t.Fatal("incomplete state was reported as a successful sample")
			}
		})
	}
}

func TestContainerInspectValidOneShotAndApp(t *testing.T) {
	states, err := parseContainerInspect([]byte("/init|exited|0|0|\n/app|running|1|0|healthy\n"), 2)
	if err != nil || len(states) != 2 || !states[0].ok() || !states[1].ok() || states[1].Restarts != 1 {
		t.Fatal("valid one-shot service and healthy app were not preserved")
	}
}
