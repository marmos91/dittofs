package index

import (
	"cmp"
	"math"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Retained is one retained extent: content a newer record superseded that the
// journal still keeps, for offload to offer or until the superseding record is
// synced. It is the piece as it was held, with its extent, record, skip,
// version and offloaded bit, and By, the version of the record that superseded
// it. A part a failed window leaves retained keeps the failed record's version
// (see failWindow).
//
// The superseder is named by version, not by where its record sits: a record
// re-appended after a failed sync, or copied by repack, moves but keeps its
// version, and no version is issued to two records.
type Retained struct {
	Piece
	By core.Version
}

// WindowRecord names one record of a failed sync window: where it sits and its
// version.
type WindowRecord struct {
	Record  RecordAt
	Version core.Version
}

// retainedSet is one file's retained extents. Unlike the held set, its pieces
// may overlap each other and the held extents, and none of them ever answers a
// read: a read consults the held set only, which this set is kept beside, not
// inside. Pieces are kept in offset, then version, then superseder order.
//
// Like the held set, it is changed only under the file's exclusive lock, and
// its scratch slices belong to those changes.
//
// ponytail: a sorted slice, so finding the pieces of a record, a superseder or
// an extent scans all r of them, and an insert moves up to r. Retained content
// is one sync window's superseded pieces plus what snapshot holds keep, and no
// query of it is on the read path. Upgrade to a B-tree keyed on (offset,
// version) with an index by superseder when a hold over a large dirty file
// shows these scans in a profile.
type retainedSet struct {
	pieces []Retained
	parts  []Retained // the pieces an edit puts back
}

func compareRetained(a, b Retained) int {
	return cmp.Or(
		cmp.Compare(a.Extent.Off, b.Extent.Off),
		a.Version.Compare(b.Version),
		a.By.Compare(b.By),
		cmp.Compare(a.Record.Segment, b.Record.Segment),
		cmp.Compare(a.Record.Offset, b.Record.Offset),
		cmp.Compare(a.Skip, b.Skip),
	)
}

// len returns the number of retained pieces.
func (s *retainedSet) len() int { return len(s.pieces) }

// add retains r. An empty piece is not kept.
func (s *retainedSet) add(r Retained) {
	if r.Extent.Len <= 0 {
		return
	}
	i, _ := slices.BinarySearchFunc(s.pieces, r, compareRetained)
	s.pieces = slices.Insert(s.pieces, i, r)
}

// list appends every retained piece to out, in order. It costs O(r).
func (s *retainedSet) list(out []Retained) []Retained {
	return append(out, s.pieces...)
}

// in appends to out the retained pieces whose record is in seg, in order.
// Repack asks it while reading seg, so no reverse index from segments is kept.
func (s *retainedSet) in(seg core.SegmentID, out []Retained) []Retained {
	for _, r := range s.pieces {
		if r.Record.Segment == seg {
			out = append(out, r)
		}
	}
	return out
}

// bySuperseder selects the pieces the record of version v superseded.
func bySuperseder(v core.Version) func(Retained) bool {
	return func(r Retained) bool { return r.By == v }
}

// drop stops retaining the parts inside e of the pieces match selects,
// splitting a piece at e's ends, and appends each part dropped to out. Which
// retention has ended — a superseding record synced, an offload commit
// reported, a hold let go — is the caller's to select.
func (s *retainedSet) drop(e core.Extent, match func(Retained) bool, out []Retained) []Retained {
	s.edit(e, match, func(r Retained) (Retained, bool) {
		out = append(out, r)
		return r, false
	})
	return out
}

// repoint moves the parts inside e of the pieces retained from record from to
// record to, whose payload holds e's bytes from skip on, keeping their
// versions, bits and superseders: repack carries a retained record forward
// like held content, and a re-appended record is the same record in a new
// place. It returns the number of pieces moved.
func (s *retainedSet) repoint(e core.Extent, from, to RecordAt, skip uint32) int {
	n := 0
	s.edit(e, func(r Retained) bool { return r.Record == from }, func(r Retained) (Retained, bool) {
		r.Record, r.Skip = to, skip+uint32(r.Extent.Off-e.Off)
		n++
		return r, true
	})
	return n
}

// edit calls fn on the part inside e of every piece match selects, and puts
// back what fn returns when it reports true, along with the parts outside e.
// fn is called in order, and may change anything but the part's extent.
func (s *retainedSet) edit(e core.Extent, match func(Retained) bool, fn func(Retained) (Retained, bool)) {
	if e.Len <= 0 {
		return
	}
	end := e.Off + e.Len
	s.parts = s.parts[:0]
	kept := s.pieces[:0]
	for _, r := range s.pieces {
		rend := r.Extent.Off + r.Extent.Len
		if rend <= e.Off || r.Extent.Off >= end || !match(r) {
			kept = append(kept, r)
			continue
		}
		if r.Extent.Off < e.Off {
			head := r
			head.Extent.Len = e.Off - r.Extent.Off
			s.parts = append(s.parts, head)
		}
		if q, ok := fn(Retained{Piece: clip(r.Piece, e), By: r.By}); ok {
			s.parts = append(s.parts, q)
		}
		if rend > end {
			s.parts = append(s.parts, Retained{Piece: clip(r.Piece, core.Extent{Off: end, Len: rend - end}), By: r.By})
		}
	}
	clear(s.pieces[len(kept):])
	s.pieces = kept
	for _, q := range s.parts {
		s.add(q)
	}
}

// whole is every offset a file can hold.
var whole = core.Extent{Off: 0, Len: math.MaxInt64}

// retainCut stops holding e, and retains each piece it held there as
// superseded by the record of version by. A gap retains nothing.
func (f *file) retainCut(e core.Extent, by core.Version) {
	f.held.visit(e, func(p Piece) bool {
		f.retained.add(Retained{Piece: clip(p, e), By: by})
		return true
	})
	f.held.cut(e)
}

// writeRetain holds p as a client write, like write, and retains what it
// supersedes. Whether a write retains is the caller's choice.
func (f *file) writeRetain(p Placed) {
	if p.Extent.Len <= 0 {
		return
	}
	f.retainCut(p.Extent, p.Version)
	f.write(p)
}

// failWindow drops a failed sync window's records from the file and holds
// again what they superseded, with its own record, skip, version and bit, so
// a read serves the last synced write. It takes the whole window, in any
// order, and fails its records newest version first: a record's retained
// pieces are held again exactly where that record is still held, which for
// two overlapping records of one window is true of the older only once the
// newer has been failed. The caller's slice is not reordered.
//
// For each record it stops holding the record's pieces, drops the pieces
// retained from it, since the record is gone, and holds again the pieces it
// superseded wherever it was held. A part it superseded that a surviving,
// newer write has since superseded is not held again: as if the failed record
// had never been written, that newer write supersedes it. Such parts stay
// retained, and are appended to out for the caller, which knows whether
// anything still keeps them.
//
// decision: a part left retained keeps the failed record's version as its By,
// though that record will never sync, so selecting by superseder on a later
// sync never drops it: the caller must drop what out names, or keep it under
// a hold, by its own selection. The index does not re-attribute it to the
// surviving write over it, because that write may cover it in several records,
// or have been released, and only the caller knows which retention, if any,
// still applies. Revisit if callers end up re-attributing every reported part
// themselves.
//
// The change version is left where the failed writes raised it: it never
// goes down.
//
// ponytail: each failed record walks the file's held pieces to find its own,
// O(n) per record, because the held set has no index by record. A window is
// rare and short; pass each record's extent with it when a failed window over
// files with many extents shows the walk in a profile.
func (f *file) failWindow(window []WindowRecord, out []Retained) []Retained {
	order := slices.Clone(window)
	slices.SortFunc(order, func(a, b WindowRecord) int { return b.Version.Compare(a.Version) })
	var was []core.Extent
	for _, w := range order {
		was = was[:0]
		f.held.visit(whole, func(p Piece) bool {
			if p.Record == w.Record {
				was = append(was, p.Extent)
			}
			return true
		})
		for _, e := range was {
			f.held.cut(e)
		}
		f.retained.edit(whole, func(r Retained) bool { return r.Record == w.Record }, func(r Retained) (Retained, bool) {
			return r, false
		})
		by := bySuperseder(w.Version)
		for _, e := range was {
			f.retained.edit(e, by, func(r Retained) (Retained, bool) {
				f.held.put(r.Piece)
				return r, false
			})
		}
	}
	// What is left superseded by a record of the window is reported once
	// every record has failed: a part one record's pass leaves can be a piece
	// of an older record of the window, which that record's own pass drops.
	for _, r := range f.retained.pieces {
		if slices.ContainsFunc(window, func(w WindowRecord) bool { return w.Version == r.By }) {
			out = append(out, r)
		}
	}
	return out
}
