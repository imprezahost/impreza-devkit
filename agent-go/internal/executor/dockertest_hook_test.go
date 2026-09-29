package executor

import "github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"

// A test binary started through the docker double's shim acts as docker.
func init() { dockertest.MaybeRun() }
