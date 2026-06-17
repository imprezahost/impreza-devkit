//go:build !linux

// Stub for non-Linux dev builds. The real agent only runs on Linux
// (systemd unit, /var/lib/impreza-agent, /proc reads); this file
// exists so `go build ./...` / `go vet` / IDE checks pass on
// Windows + macOS developer machines.

package sysload

import (
	"errors"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// ErrUnsupported is returned by Collect on non-Linux builds.
var ErrUnsupported = errors.New("sysload: only supported on Linux")

// Collect returns nil + ErrUnsupported on non-Linux. The poller's
// heartbeat handler logs the error once and continues without a
// load field — the AgentReport schema marks Load as optional.
func Collect() (*sdkclient.AgentLoad, error) {
	return nil, ErrUnsupported
}
