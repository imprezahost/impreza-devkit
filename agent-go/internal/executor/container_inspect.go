package executor

import (
	"fmt"
	"strconv"
	"strings"
)

type containerState struct {
	Name     string
	Status   string // running | restarting | exited | created | paused | dead
	Restarts int
	ExitCode int
	Health   string // "", starting, healthy, unhealthy
}

// ok reports whether this container counts as settled.
//
// An `exited` container with code 0 is treated as fine on purpose: a
// stack may include a one-shot init/migration service that does its job
// and stops, and failing a deploy over that would be wrong.
//
// `unhealthy` is NOT ok, but it is also not fatal — it feeds the
// advisory verdict, because images differ wildly in how long they take
// to report healthy on a cold first boot.
func (c containerState) ok() bool {
	switch c.Status {
	case "running":
		return c.Health == "" || c.Health == "healthy"
	case "exited":
		return c.ExitCode == 0
	default:
		return false
	}
}

func parseContainerInspect(out []byte, expected int) ([]containerState, error) {
	states := make([]containerState, 0, expected)
	seen := make(map[string]bool, expected)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 5 {
			return nil, fmt.Errorf("docker inspect: malformed container row")
		}
		name := strings.TrimPrefix(parts[0], "/")
		if name == "" || parts[1] == "" || seen[name] {
			return nil, fmt.Errorf("docker inspect: missing or duplicate container identity or status")
		}
		restarts, restartErr := strconv.Atoi(strings.TrimSpace(parts[2]))
		exitCode, exitErr := strconv.Atoi(strings.TrimSpace(parts[3]))
		if restartErr != nil || exitErr != nil || restarts < 0 || exitCode < 0 {
			return nil, fmt.Errorf("docker inspect: invalid restart count or exit code")
		}
		seen[name] = true
		states = append(states, containerState{
			Name: name, Status: parts[1], Restarts: restarts, ExitCode: exitCode, Health: parts[4],
		})
	}
	if len(states) != expected {
		return nil, fmt.Errorf("docker inspect: expected %d containers, got %d", expected, len(states))
	}
	return states, nil
}
