//go:build !windows

package run

import (
	"os/exec"
	"syscall"
)

// killTree makes the command's cancellation kill its whole process group, so
// nothing it started outlives it.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
