package prof

import (
	"os"
	"strconv"
)

// descriptors returns the process's open descriptors, each with the file it
// refers to, and true: Linux lists them in /proc/self/fd.
func descriptors() (map[int]string, bool) {
	fds := listDescriptors("/proc/self/fd")
	for fd := range fds {
		// A descriptor closed since the listing has no link left; it keeps
		// its empty name.
		target, _ := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		fds[fd] = target
	}

	return fds, true
}
