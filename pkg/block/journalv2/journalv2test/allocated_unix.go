//go:build unix

package journalv2test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// allocated returns the storage the filesystem has allocated to the regular
// files directly inside dir: their blocks, which a sparse or preallocated
// file's size does not show. The format file, and the temporary one that
// replaces it, are left out: they hold the journal's identity, not the
// segments a journal's accounting covers.
func allocated(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}

	var n int64
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == "format" || e.Name() == "format.tmp" {
			continue
		}

		// A file renamed or removed since the listing, as a spare becoming a
		// segment is, holds nothing under the name listed.
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(dir, e.Name()), &st); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return 0, err
		}
		// st_blocks counts 512-byte units on every unix, whatever the
		// filesystem's block size.
		n += st.Blocks * 512
	}
	return n, nil
}
