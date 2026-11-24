//go:build !windows

package extool

import (
	"os/exec"
	"syscall"
)

// procGroup is the process group of a child started by setGroup.
type procGroup struct{ cmd *exec.Cmd }

// setGroup makes the child the leader of a new process group and makes
// cancelling the command kill the whole group.
func setGroup(cmd *exec.Cmd) procGroup {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return procGroup{cmd}
}

// killAll kills what is left of the group.
func (g procGroup) killAll() {
	if g.cmd.Process != nil {
		syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
	}
}
