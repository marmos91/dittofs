package run

import (
	"os/exec"
	"time"
)

// killTree keeps the default cancellation, which kills the command itself.
func killTree(*exec.Cmd) {}

// killLeftovers does nothing: what a command leaves behind isn't tracked here.
func killLeftovers(*exec.Cmd) {}

// interruptTree keeps the default cancellation; nothing runs through sudo here.
func interruptTree(cmd *exec.Cmd, grace time.Duration) { cmd.WaitDelay = grace }
