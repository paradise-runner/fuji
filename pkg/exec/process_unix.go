//go:build !windows

package exec

import (
	"os/exec"
	"syscall"
	"time"
)

// killGracePeriod is how long to wait between SIGTERM and SIGKILL when
// terminating a process group.
const killGracePeriod = 200 * time.Millisecond

// setupProcessGroup places the child in its own process group so that timeout
// / abort can terminate the child and all of its descendants together.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}
}

// signalProcessGroup sends sig to every process in the group led by pid
// (negative pgid). It is a no-op on a nonexistent group.
func signalProcessGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}

// killProcessGroup terminates the process group led by pid, escalating from
// SIGTERM to SIGKILL after a short grace period.
func killProcessGroup(pid int) {
	signalProcessGroup(pid, syscall.SIGTERM)
	select {
	case <-time.After(killGracePeriod):
		signalProcessGroup(pid, syscall.SIGKILL)
	}
}
