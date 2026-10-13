package run

import "os/exec"

// killTree keeps the default cancellation, which kills the command itself.
func killTree(*exec.Cmd) {}
