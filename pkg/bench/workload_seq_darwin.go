//go:build darwin

package bench

import (
	"os"
	"syscall"
)

// disableCache sets F_NOCACHE on macOS to bypass the unified buffer cache.
func disableCache(f *os.File) {
	// F_NOCACHE = 48 on darwin.
	syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), 48, 1) //nolint:errcheck
}
