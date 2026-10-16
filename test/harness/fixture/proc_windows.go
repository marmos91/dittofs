package fixture

import "os/exec"

// ownGroup does nothing: processes here have no groups.
func ownGroup(*exec.Cmd) {}

// stopGroup ends dfs, which can't be asked to stop here.
func stopGroup(cmd *exec.Cmd, _ bool) {
	_ = cmd.Process.Kill()
}
