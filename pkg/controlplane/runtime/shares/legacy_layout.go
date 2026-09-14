package shares

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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
	// Everything below is read THROUGH an os.Root anchored at shareDir, so no
	// name resolved here can leave that directory even if one of its entries is
	// a symlink pointing elsewhere. The names joined onto it are compile-time
	// constants, and the root is the containment the caller's configured path
	// does not carry on its own.
	root, err := os.OpenRoot(shareDir)
	if err != nil {
		// A share directory that does not exist yet is a fresh share, not a
		// legacy one: journal.Open creates it.
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = root.Close() }()

	legacy := false
	for _, sub := range []string{"blobs", "logs"} {
		d, err := root.Open(sub)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		entries, err := d.ReadDir(1) // presence is the question, not the count
		_ = d.Close()
		if err != nil && !errors.Is(err, io.EOF) {
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

	jd, err := root.Open("journal")
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = jd.Close() }()
	entries, err := jd.ReadDir(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".seg") {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if info.Size() > journalSegHeaderSize {
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
