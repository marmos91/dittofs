//go:build linux || darwin

package prof

import (
	"os"
	"strconv"
)

// listDescriptors returns the open descriptors the directory dir lists, one
// entry per descriptor, leaving out the one used to read dir itself.
func listDescriptors(dir string) map[int]string {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = d.Close() }()

	own := int(d.Fd())
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil
	}

	out := make(map[int]string, len(names))
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil || fd == own {
			continue
		}
		out[fd] = ""
	}

	return out
}
