package run

import (
	"os/exec"
	"time"
)

// killTree keeps the default cancellation, which kills the command itself.
func killTree(*exec.Cmd) {}

// interruptTree keeps the default cancellation; nothing runs through sudo here.
func interruptTree(cmd *exec.Cmd, grace time.Duration) { cmd.WaitDelay = grace }
