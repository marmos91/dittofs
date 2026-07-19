package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// legacySegHeaderSize is the size of a freshly created journal segment file:
// header only, no records. It mirrors the journal's own segHeaderSize (kept in
// sync deliberately — the journal keeps that constant unexported). A journal
// that has taken over a directory therefore leaves at least one .seg file larger
// than this once it holds real data.
const legacySegHeaderSize = 64

// ErrLegacyLocalFormat reports that a local store directory holds a pre-journal
// on-disk layout (blobs/ + logs/) the journal cannot read. Opening such a
// directory as a journal would silently start empty and serve every stored file
// as zeros, so the store refuses to open. The legacy bytes are left untouched on
// disk for a migration to recover.
var ErrLegacyLocalFormat = errors.New("legacy pre-journal local block store layout")

// hasLegacyLocalLayout reports whether dir was written by a pre-journal release:
// a populated blobs/ or logs/ subdirectory with no journal segment yet holding
// data. Post-switchover the journal writes only journal/*.seg (+ .idx), so a
// non-empty blobs/ or logs/ can only be pre-journal data. Once the journal owns
// the directory (any .seg file past the bare header) those subdirectories are
// just orphans a migration left behind, so their presence alone no longer counts
// as legacy.
func hasLegacyLocalLayout(dir string) (bool, error) {
	legacy := false
	for _, sub := range []string{"blobs", "logs"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if len(entries) > 0 {
			legacy = true
			break
		}
	}
	if !legacy {
		return false, nil
	}
	// A journal that already holds data supersedes the legacy dirs: an in-place
	// upgrade that has genuinely migrated (or a native store that happens to keep
	// an orphaned blobs/) is not legacy. Empty header-only segments (a develop
	// binary that started once over legacy data and inited an empty journal) do
	// not count as data, so the guard still fires on the second start.
	segs, err := filepath.Glob(filepath.Join(dir, "journal", "*.seg"))
	if err != nil {
		return false, err
	}
	for _, seg := range segs {
		fi, err := os.Stat(seg)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		if fi.Size() > legacySegHeaderSize {
			return false, nil
		}
	}
	return true, nil
}

// checkLegacyLayout returns a descriptive ErrLegacyLocalFormat when dir holds a
// pre-journal layout, so the caller refuses to open it as an empty journal.
func checkLegacyLayout(dir string) error {
	legacy, err := hasLegacyLocalLayout(dir)
	if err != nil {
		return err
	}
	if legacy {
		return fmt.Errorf("%w: %q holds blobs/+logs/ from a pre-journal release; "+
			"refusing to open it as an empty journal, which would serve the stored files as zeros. "+
			"The data is intact on disk — migrate it before upgrading", ErrLegacyLocalFormat, dir)
	}
	return nil
}
