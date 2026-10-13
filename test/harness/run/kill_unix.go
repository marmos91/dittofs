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

// stopTree is killTree for a command run through sudo. dt can't signal what
// sudo runs as root, so cancelling sends SIGTERM, which sudo passes on, and
// kills sudo only if it hasn't ended within grace.
func stopTree(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = grace
}
