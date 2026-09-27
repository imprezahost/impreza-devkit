//go:build linux

package executor

import (
	"os/exec"
	"syscall"
	"time"
)

// preparationWaitDelay bounds how long Run waits for output pipes that a
// surviving compose-plugin grandchild still holds after the context kill.
const preparationWaitDelay = 5 * time.Second

// prepareWorkerCommand hardens a supervised preparation command so the
// budget's cancellation actually ends it: the docker CLI runs
// compose as a plugin child, and killing only the CLI leaves the plugin
// alive holding the output pipes — Run() then never returns and the worker
// writes no receipt. The command runs in its own process group and the
// cancel kills the whole group; WaitDelay bounds the remaining pipe wait.
func prepareWorkerCommand(command *exec.Cmd) {
	command.WaitDelay = preparationWaitDelay
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
			return command.Process.Kill()
		}
		return nil
	}
}
