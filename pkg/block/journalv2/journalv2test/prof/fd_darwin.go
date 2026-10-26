package prof

// descriptors returns the process's open descriptors from /dev/fd, and whether
// that could be read. macOS gives no file for them, so their names stay empty.
func descriptors() (map[int]string, bool) {
	return listDescriptors("/dev/fd")
}
