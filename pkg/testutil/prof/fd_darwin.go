package prof

// descriptors returns the process's open descriptors and true: macOS lists
// them in /dev/fd. It gives no file for them, so their names stay empty.
func descriptors() (map[int]string, bool) {
	return listDescriptors("/dev/fd"), true
}
