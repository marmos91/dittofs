package model

import (
	"context"
	"iter"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// View is what Compare reads from the journal for one share: the calls a
// caller makes on the share's handle, and the journal's index of a file.
// HandleView serves it from a real journal; a test can serve it from anything
// else, such as a wrapper that plants a divergence.
type View interface {
	ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (n int, missing []journal.Extent, held []journal.Held, asOf journal.Version, err error)
	Extents(id journal.FileID) ([]journal.Extent, error)
	Since(ctx context.Context, id journal.FileID, from journal.Version) iter.Seq2[journal.Change, error]
	Files() iter.Seq[journal.FileID]
	DirtyFiles() iter.Seq2[journal.FileID, time.Time]

	// Index returns the journal's index entries of file id, which carry the
	// offloaded bit of each held extent. The handle never answers with the
	// bit, so it is read here, from the journal's inspection.
	Index(id journal.FileID) ([]journal.IndexEntry, error)
}

// HandleView is the View of one share of a journal: its handle, and the
// journal's inspection for the index.
type HandleView struct {
	*journal.Handle
	Journal *journal.Journal
}

// Index returns the index entries of file id from the journal's inspection.
func (v HandleView) Index(id journal.FileID) ([]journal.IndexEntry, error) {
	return v.Journal.Inspect().File(id)
}
