package prof

import (
	"os"
	"strconv"
)

// descriptors returns the process's open descriptors, each with the file it
// refers to, from /proc/self/fd, and whether that could be read.
func descriptors() (map[int]string, bool) {
	fds, ok := listDescriptors("/proc/self/fd")
	for fd := range fds {
		// A descriptor closed since the listing has no link left; it keeps
		// its empty name.
		target, _ := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		fds[fd] = target
	}

	return fds, ok
}
