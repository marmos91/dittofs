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
// the store refuses to open and the legacy bytes stay intact on disk.
//
// Refusing is not merely tidier than mounting empty. A local-only share's local
// bytes are its only copy, and once that share has been compacted the bytes are
// unrecoverable from the directory alone: the index that locates a chunk inside
// blobs/ lives in the metadata store and is dropped by its migrations, while
// blobs/ itself is raw unframed concatenation with nothing to scan for. Leaving
// the directory untouched and stopping loudly is what keeps a backup usable.
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
	// Defense-in-depth, matching the check the fs branch already makes on the
	// configured base path: the caller resolved shareDir from a stored config,
	// and a relative one would make the joins below resolve against the
	// server's CWD and scan a directory nobody named. Every component joined
	// onto it here is a compile-time constant, so an absolute, cleaned root is
	// the whole of what this needs to be safe.
	if !filepath.IsAbs(shareDir) {
		return false, fmt.Errorf("share dir must be absolute, got %q", shareDir)
	}
	shareDir = filepath.Clean(shareDir)

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
