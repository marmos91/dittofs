//go:build windows

package engine

import (
	"fmt"
	"os"
)

// readFromCAS reads up to len(dest) bytes from path, starting at the
// given byte offset, into dest. It returns the number of bytes copied
// (which may be less than len(dest) if the file ends first or the
// offset is beyond EOF).
//
// On Windows we use os.ReadFile (full file load) — the Windows mmap
// path (CreateFileMapping/MapViewOfFile via golang.org/x/sys/windows)
// is deferred per Phase 12 D-33: the perf gate (D-43) is rand-read on
// linux/darwin; Windows perf is not in scope for v0.15.0. Revisit if
// Windows perf complaints surface.
func readFromCAS(path string, offset uint32, dest []byte) (int, error) {
	if len(dest) == 0 {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("readfile cas chunk %s: %w", path, err)
	}
	if int(offset) >= len(data) {
		return 0, nil // offset past EOF
	}
	return copy(dest, data[offset:]), nil
}
