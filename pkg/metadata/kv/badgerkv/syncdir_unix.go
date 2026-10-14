//go:build !windows

package badgerkv

import "os"

// syncDir syncs a directory, which makes the entry of a file created in it
// durable.
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
