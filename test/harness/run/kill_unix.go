//go:build !windows

package run

import (
	"os/exec"
	"syscall"
	"time"
)

// killTree makes the command's cancellation kill its whole process group, so
// nothing it started outlives it.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// interruptTree is killTree for a command whose tests run as root, which dt
// can't kill: cancelling sends SIGINT to the process group, which sudo passes
// on to the tests so their cleanup runs, and kills the command only if it
// hasn't ended within grace.
func interruptTree(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
	cmd.WaitDelay = grace
}
