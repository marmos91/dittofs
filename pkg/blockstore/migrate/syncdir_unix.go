//go:build !windows

package migrate

import "os"

// syncDir fsyncs the parent directory so a prior os.Rename's metadata
// reaches disk. POSIX requires this for crash-durable atomic-rename;
// Windows does not (and opening a dir handle there can race the file
// truncate that immediately follows in snapshotLocked).
func syncDir(dir string) {
	df, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = df.Sync()
	_ = df.Close()
}
