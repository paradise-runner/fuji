//go:build windows

package exec

import (
	"os/exec"
	"syscall"
)

// Windows does not handle timeout termination via POSIX process groups; the
// os/exec.CommandContext default (kill the direct child on cancel) is used.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
