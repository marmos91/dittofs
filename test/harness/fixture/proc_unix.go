//go:build !windows

package fixture

import (
	"os/exec"
	"syscall"
)

// ownGroup starts dfs in a process group of its own: stopping that group
// stops what dt started and nothing else, and a terminal's interrupt reaches
// dt, which then stops it. Where the system can, dfs also ends with dt.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	endWithParent(cmd.SysProcAttr)
}

// stopGroup sends dfs's group SIGTERM, or SIGKILL when kill is set.
func stopGroup(cmd *exec.Cmd, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
