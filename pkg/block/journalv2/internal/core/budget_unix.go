//go:build unix

package core

import "syscall"

// platformOpenFileLimit returns the process's open-file limit, and whether the
// platform reports one. It reports false when the limit cannot be read, when
// it is zero, or when it is implausibly large: RLIM_INFINITY is an untyped
// constant whose value differs per platform — darwin says 1<<63-1, Linux and
// the BSDs say ^uint64(0), illumos says something else — so any value of that
// magnitude is treated as unlimited, and no finite bound can be a quarter of
// infinity: the default stands in instead.
func platformOpenFileLimit() (uint64, bool) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, false
	}
	cur := uint64(rl.Cur)
	if cur == 0 || cur >= 1<<62 {
		return cur, false
	}
	return cur, true
}
