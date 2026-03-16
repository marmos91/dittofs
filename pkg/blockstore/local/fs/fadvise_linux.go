package fs

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropPageCache advises the kernel to drop page cache for the given file.
// This prevents .blk files from consuming RAM in the OS page cache.
func dropPageCache(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
