package fixture

import "syscall"

// endWithParent has the kernel kill dfs when dt ends, also when dt panics or
// is killed and never stops it. The kernel sends the signal when the thread
// that started dfs ends, which in Go is when dt does: dt locks no goroutine to
// its thread.
func endWithParent(a *syscall.SysProcAttr) {
	a.Pdeathsig = syscall.SIGKILL
}
