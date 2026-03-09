//go:build linux

package bench

import (
	"os"

	"golang.org/x/sys/unix"
)

// disableCache uses posix_fadvise(FADV_DONTNEED) to advise the kernel to
// drop cached pages for this file. This is the Linux equivalent of macOS
// F_NOCACHE, ensuring sequential read benchmarks measure actual I/O
// throughput rather than page cache hits.
func disableCache(f *os.File) {
	// FADV_DONTNEED: drop any existing cached pages for this file.
	// Errors are ignored — this is best-effort (some filesystems may not support it).
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
