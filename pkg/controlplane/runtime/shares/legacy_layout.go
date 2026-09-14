package shares

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// journalSegHeaderSize is the size of a freshly created journal segment file:
// header only, no records. It mirrors the journal's own segHeaderSize, which
// that package keeps unexported — deliberately duplicated rather than exported,
// since journal depends on nothing outside the standard library and must not
// learn what a DittoFS share directory looks like.
const journalSegHeaderSize = 64

// ErrLegacyLocalFormat reports that a share directory holds a pre-journal
// on-disk layout (blobs/ + logs/) that this build cannot read. Opening such a
// directory as a journal starts empty and serves every stored file as zeros, so
// the store refuses to open and the legacy bytes stay intact on disk for a
// migration to recover.
//
// This restores the #1802 guard, which pkg/block/local/fs carried until that
// package was deleted. The failure it prevents is not hypothetical: it is the
// reported #1801 incident, "upgrade from v0.26.0 silently loses local block
// data". For a compacted local-only share the loss is permanent — the
// blobs/ chunk index (local_chunk_index) is dropped by metadata migrations, and
// the blobs themselves are raw unframed concatenations, so nothing can locate a
// chunk in them afterwards. Refusing loudly and leaving the bytes untouched is
// the only thing that keeps a pre-upgrade backup usable.
var ErrLegacyLocalFormat = errors.New("legacy pre-journal local block store layout")

// hasLegacyLocalLayout reports whether shareDir was written by a pre-journal
// release: a populated blobs/ or logs/ subdirectory with no journal segment yet
// holding data. Post-switchover the journal writes only journal/*.seg (+ .idx),
// so a non-empty blobs/ or logs/ can only be pre-journal data. Once the journal
// owns the directory (any .seg past the bare header) those subdirectories are
// orphans a migration left behind, and their presence alone no longer counts.
//
// Header-only segments deliberately do NOT count as data. A build without this
// guard opens a legacy directory once and stamps an empty journal beside the
// untouched bytes; treating that stamp as ownership would make the guard fire
// only for sites that had never started the broken build. It fires on the
// second start too, which is the case that actually reaches an operator.
func hasLegacyLocalLayout(shareDir string) (bool, error) {
	legacy := false
	for _, sub := range []string{"blobs", "logs"} {
		entries, err := os.ReadDir(filepath.Join(shareDir, sub))
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

	segs, err := filepath.Glob(filepath.Join(shareDir, "journal", "*.seg"))
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
		if fi.Size() > journalSegHeaderSize {
			return false, nil
		}
	}
	return true, nil
}

// checkLegacyLayout returns a descriptive ErrLegacyLocalFormat when shareDir
// holds a pre-journal layout, so the caller refuses to open it as an empty
// journal.
func checkLegacyLayout(shareDir string) error {
	legacy, err := hasLegacyLocalLayout(shareDir)
	if err != nil {
		return err
	}
	if legacy {
		return fmt.Errorf("%w: %q holds blobs/+logs/ from a pre-journal release; "+
			"refusing to open it as an empty journal, which would serve the stored files as zeros. "+
			"The data is intact on disk — migrate it before upgrading", ErrLegacyLocalFormat, shareDir)
	}
	return nil
}
