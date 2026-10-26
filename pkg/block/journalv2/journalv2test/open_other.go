//go:build !windows

package journalv2test

import "os"

// openFile opens path for the seam. Outside Windows an open file can be
// renamed and removed as it is, so os.OpenFile is all it needs.
func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
