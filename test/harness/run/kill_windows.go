package run

import "os/exec"

// killTree keeps the default cancellation, which kills the command itself.
func killTree(*exec.Cmd) {}

// killLeftovers does nothing: what a command leaves behind isn't tracked here.
func killLeftovers(*exec.Cmd) {}
