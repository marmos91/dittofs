//go:build !windows

package fs

import "os"

// fsyncDir opens path, fsyncs it, and closes the descriptor.
// Unix filesystems need this to durably persist directory entries
// after mkdir/create/rename.
func fsyncDir(path string) error {
	d, err := os.Open(path) //nolint:gosec // path is driver-constructed
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}
