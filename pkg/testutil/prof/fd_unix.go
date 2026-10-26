//go:build linux || darwin

package prof

import (
	"errors"
	"os"
	"strconv"
	"syscall"
)

// listDescriptors returns the open descriptors the directory dir lists, one
// entry per descriptor, and whether dir could be read.
//
// Reading dir opens descriptors of its own, and on macOS more than one: the
// directory's, and a duplicate of it the listing reads through. Their numbers
// depend on which lower numbers are free, so they cannot be told apart from a
// test's by number. They are all closed before the listing is filtered, and
// only descriptors still open then are kept.
func listDescriptors(dir string) (map[int]string, bool) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, false
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return nil, false
	}

	out := make(map[int]string, len(names))
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil || !isOpen(fd) {
			continue
		}
		out[fd] = ""
	}

	return out, true
}

// isOpen reports whether fd is an open descriptor.
func isOpen(fd int) bool {
	var st syscall.Stat_t
	return !errors.Is(syscall.Fstat(fd, &st), syscall.EBADF)
}
