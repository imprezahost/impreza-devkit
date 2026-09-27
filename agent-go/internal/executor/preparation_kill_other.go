//go:build !linux

package executor

import (
	"os/exec"
	"time"
)

// preparationWaitDelay bounds how long Run waits for output pipes that a
// surviving plugin child still holds after the context kill.
const preparationWaitDelay = 5 * time.Second

// prepareWorkerCommand applies the portable half of the supervised-worker
// hardening. Supervised preparation only ever runs on Linux;
// elsewhere the default kill of the direct child is all there is.
func prepareWorkerCommand(command *exec.Cmd) {
	command.WaitDelay = preparationWaitDelay
}
