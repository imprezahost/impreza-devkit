//go:build !linux

package ingress

import "sync"

var lockMu sync.Mutex

// lockState is process-local off Linux (tests); the agent only runs on Linux.
func lockState(string) (func(), error) {
	lockMu.Lock()
	return lockMu.Unlock, nil
}
