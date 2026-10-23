package journalv2

import (
	"context"
	"iter"
)

// Inspect returns a way to read out the journal's placement index, per file
// and in bulk, read its per-segment accounting, and verify the index against
// the segments. None of it mutates the journal or stops it.
func (j *Journal) Inspect() Inspector {
	return Inspector{}
}

// Inspector reads out a journal's index and segments. Its methods are safe for
// concurrent use with every other operation on the journal. After the journal
// closes, File and Verify fail with ErrClosed, and All and Segments return
// nothing.
//
// decision: All and Segments have no error to return, so after Close they
// return nothing rather than a view of state the journal has released.
type Inspector struct{}

// File returns the index entries of file id, in offset order.
func (in Inspector) File(id FileID) ([]IndexEntry, error) {
	return nil, ErrNotImplemented
}

// All yields the index entries of every file in the index, one file at a time.
func (in Inspector) All() iter.Seq2[FileID, []IndexEntry] {
	return func(func(FileID, []IndexEntry) bool) {}
}

// Segments returns the accounting of every segment the journal owns. Stats
// gives the totals; this is the breakdown behind them.
func (in Inspector) Segments() []SegmentInfo {
	return nil
}

// Verify checks the index against the segments and reports what does not
// agree. It reports and never repairs: repair is a separate, explicit action.
//
// Verify checks each file against a stable view of that file, so a write in
// flight during the pass is never reported as a disagreement. It reads every
// segment, so it takes a context, honoured as the package doc states.
func (in Inspector) Verify(ctx context.Context) (VerifyReport, error) {
	return VerifyReport{}, ErrNotImplemented
}

// EntryKind is what an index entry records about its extent.
type EntryKind uint8

// The entry kinds.
const (
	// EntryHeld is an extent the journal holds: a read serves it.
	EntryHeld EntryKind = iota + 1

	// EntryRetained is superseded content kept until its superseding write is
	// synced, or until its successor's existence commits, or while a snapshot
	// hold keeps it. A read does not serve it.
	EntryRetained

	// EntryMarker is a removal not yet settled: a truncate, deallocate or
	// delete the caller has not settled.
	EntryMarker
)

// IndexEntry is one entry of a file's placement index.
type IndexEntry struct {
	Kind    EntryKind
	Extent  Extent
	Version Version

	// Tag is the tag of the share whose record backs the entry.
	Tag ShareTag

	// Location is the record holding the entry's bytes; zero for a marker.
	//
	// decision: an entry is one contiguous piece of one record, so its
	// location is that record's, the same RecordRef Verify reports.
	Location RecordRef

	// Offloaded is the entry's offloaded bit. It is never an answer about
	// whether the content is offloaded: the journal is not authoritative for
	// that.
	Offloaded bool
}

// RecordRef locates a record: the segment holding it and its offset there.
type RecordRef struct {
	Segment SegmentID
	Offset  int64
}

// SegmentInfo is one segment's accounting.
type SegmentInfo struct {
	ID     SegmentID
	Stream uint32
	Sealed bool

	// Failed reports that a sync or an append into the segment failed, so it
	// takes no further append.
	Failed bool

	// Allocated is the storage the segment is accounted at, in bytes.
	Allocated int64

	// HeldBytes is the bytes of held extents in the segment, and
	// RetainedBytes the bytes of retained ones; neither includes the other.
	HeldBytes     int64
	RetainedBytes int64

	// Damaged reports that the segment holds records that do not verify.
	Damaged bool
}

// VerifyReport is what Verify found.
type VerifyReport struct {
	// Unsupported holds, per file, the index entries no record on the
	// segments supports.
	Unsupported map[FileID][]IndexEntry

	// Unindexed are the records that need an index entry, held, retained or a
	// removal marker, but have none. A record nothing needs any more, such as
	// one superseded and past its retention or one released, is not one.
	//
	// decision: this is narrower than RFC 1 §4.4, which asks for every record
	// the index does not reference. Superseded and released records stay on
	// disk unreferenced until repack recovers them, so on a healthy journal
	// that set is never empty and reports nothing wrong; what it would add is
	// already counted by Stats.UnreclaimedBytes. Overturned if a repair needs
	// the full unreferenced set, and then this field is widened rather than a
	// second one added.
	Unindexed []UnindexedRecord

	// BadFooters are the segments whose footer did not verify.
	BadFooters []SegmentID

	// FootprintMismatch is the accounted footprint minus the sum of the
	// segments' sizes, in bytes; zero when the two agree.
	FootprintMismatch int64
}

// UnindexedRecord is a record that needs an index entry but has none.
type UnindexedRecord struct {
	File    FileID
	Extent  Extent
	Version Version
	Record  RecordRef
}
