//go:build !windows && !linux

package fixture

import "syscall"

// endWithParent does nothing: this system has no signal on a parent's end,
// so a dfs whose dt was killed keeps running until it is stopped by hand.
func endWithParent(*syscall.SysProcAttr) {}
