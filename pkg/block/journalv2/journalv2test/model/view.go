package model

import (
	"iter"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// View is what Compare reads from the journal for one share: the calls a
// caller makes on the share's handle, and the journal's index of a file.
// HandleView serves it from a real journal; a test can serve it from anything
// else, such as a wrapper that plants a divergence.
type View interface {
	ReadAt(id journalv2.FileID, off int64, p []byte) (n int, missing []journalv2.Extent, held []journalv2.Held, asOf journalv2.Version, err error)
	Extents(id journalv2.FileID) ([]journalv2.Extent, error)
	Since(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error]
	Files() iter.Seq[journalv2.FileID]
	DirtyFiles() iter.Seq2[journalv2.FileID, time.Time]

	// Index returns the journal's index entries of file id, which carry the
	// offloaded bit of each held extent. The handle never answers with the
	// bit, so it is read here, from the journal's inspection.
	Index(id journalv2.FileID) ([]journalv2.IndexEntry, error)
}

// HandleView is the View of one share of a journal: its handle, and the
// journal's inspection for the index.
type HandleView struct {
	*journalv2.Handle
	Journal *journalv2.Journal
}

// Index returns the index entries of file id from the journal's inspection.
func (v HandleView) Index(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
	return v.Journal.Inspect().File(id)
}
