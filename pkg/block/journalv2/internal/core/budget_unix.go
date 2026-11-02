//go:build unix

package core

import "syscall"

// platformOpenFileLimit returns the process's open-file limit, and whether the
// platform reports one. It reports false when the limit cannot be read, when
// it is zero, or when it is unlimited: no finite bound can be a quarter of
// infinity, and the default stands in instead.
func platformOpenFileLimit() (uint64, bool) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, false
	}
	cur := uint64(rl.Cur)
	if cur == 0 || cur == uint64(syscall.RLIM_INFINITY) {
		return cur, false
	}
	return cur, true
}
