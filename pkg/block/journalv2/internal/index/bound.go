package index

import (
	"math"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// The index is bounded, and the bound refuses. The count is of entries:
// held pieces, retained extents and removal markers, of every file of the
// journal together. A content write that would raise it above the configured
// ExtentLimit is refused at once, before any I/O, with the same named
// refusal as a capacity one, naming the index; records without bytes are
// never refused by it, though they can raise the count.
//
// The count is maintained by the delta: every update of a file applies the
// difference its entries made, the admitted amount subtracted, so the count
// is exact whatever a pre-check predicted. Admission reserves an amount
// before the record's I/O, so a write that never commits is returned by
// abandon.

// writeAdds is the number of entries a write, or a writeRetain when retain,
// of e would add, and negative when it would drop more than it adds. It is
// one for the new placement, less the held pieces e covers whole, plus one
// where e splits a piece in three, plus, for a writeRetain, one retained
// extent per held piece e overlaps, whole or in part — retainCut retains
// one piece per piece it supersedes, wherever the write's ends fall, so a
// piece it covers whole trades its held entry for a retained one, not none.
// A zero-length e adds nothing. It reads only, so it runs under the file's
// read lock.
//
// A write never joins a neighbour at run time: its version is above every
// piece it replaces, so joins' test never holds, and the count is exact.
// A caller that names one record with placements that join — only a test —
// over-counts by the joins, and commit's true delta takes the difference
// back.
func (f *file) writeAdds(e core.Extent, retain bool) int64 {
	if e.Len <= 0 {
		return 0
	}
	n := int64(1)
	f.held.visit(e, func(p Piece) bool {
		inside := p.Extent.Off >= e.Off && p.Extent.Off+p.Extent.Len <= extentEnd(e)
		if inside {
			n--
		}
		if retain {
			n++ // the piece superseded stays, retained, beside the write
		}
		return true
	})
	if f.held.straddle(e) {
		n++ // the split leaves the piece's head and tail as two
	}
	return n
}

// fillAdds is the number of entries a fill of p would add: the gaps of the
// parts no marker above p's version masks. It reads only, so it runs under
// the file's read lock. Like gaps, the result is exact unless a put joins a
// neighbour of the same record, and the commit's true delta corrects for
// that.
func (f *file) fillAdds(p Placed) int64 {
	var n int64
	f.fillParts(p, func(part Placed) {
		n += f.held.gaps(part.Extent)
	})
	return n
}

// splits reports whether removing e would split one held piece in two: one
// piece covers both of e's ends. It costs one descent, and reads only.
func (f *file) splits(e core.Extent) bool {
	return f.held.straddle(e)
}

// admitWrite reserves the entries a write, or a writeRetain when retain, of e
// on id would add, and reports them for the caller to hand to commit, or to
// abandon once its I/O has failed. It is refused with a NoSpaceError naming
// the index when the reservation would raise the count above the limit — a
// write that adds nothing included, once the count is above the limit, since
// over the bound the count must come down and every content write is refused
// until it has. A refused admission reserves nothing.
//
// decision: the admission runs under the file's read lock, and the engine
// holds the file's append lock from a write's admission to its commit, so
// the file's content cannot change between the two — except by a release or
// an offload report on the same file, which can make the committed delta
// differ from the admitted one. commit applies the true delta, so the count
// never drifts, and it overshoots the bound only by such a race. Revisit if
// the engine ever lets releases run between another file op's admission and
// its commit and the count is seen above the bound.
func (fs *files) admitWrite(id core.FileID, e core.Extent, retain bool) (int64, error) {
	// A file with no entry holds nothing: a write of bytes adds one entry
	// and a zero-length one none.
	n := int64(1)
	if e.Len <= 0 {
		n = 0
	}
	fs.view(id, func(f *file) { n = f.writeAdds(e, retain) })
	return fs.reserve(max(n, 0))
}

// admitFill reserves the entries a fill of p on id would add. It is refused
// as admitWrite is, and races as admitWrite does.
func (fs *files) admitFill(id core.FileID, p Placed) (int64, error) {
	// A file with no entry holds nothing, so a fill of bytes occupies a whole
	// absent extent — one entry — and a zero-length one occupies none.
	n := int64(0)
	if p.Extent.Len > 0 {
		n = 1
		fs.view(id, func(f *file) { n = f.fillAdds(p) })
	}
	return fs.reserve(max(n, 0))
}

// reserve adds want to the count when the limit allows it, or refuses with a
// NoSpaceError naming the index, counting the refusal. A reservation of
// nothing is refused while the count stands above the limit — the first
// clause covers it, since want is 0 — because nothing that adds nothing can
// lower the count. Missing counts what the reservation lacked, at least one
// entry so a caller draining the bound is told a real shortfall.
func (fs *files) reserve(want int64) (int64, error) {
	for {
		c := fs.count.Load()
		limit := fs.limit.Load()
		if c+want > limit {
			fs.refusals.Add(1)
			return 0, &core.NoSpaceError{Limit: core.LimitIndex, Missing: c + max(want, 1) - limit, Unit: core.UnitEntries}
		}
		if fs.count.CompareAndSwap(c, c+want) {
			return want, nil
		}
	}
}

// admitRemoval reserves the entries a removal that would split a held extent
// adds: one for a release, which splits a piece — a release leaves no
// marker — and two for a deallocate or a clone target, which split a piece
// and leave one. It is never refused
// when e splits nothing: a removal that narrows or covers whole pieces only
// lowers the count, so refusing it would wedge the very calls that bring the
// count down. A splitting one is refused, with the same named refusal, once
// the count stands an eighth of the limit above it, so a trim pass of
// millions of punches inside held extents cannot add an entry a punch past
// any memory budget; its own refusals stop it there.
//
// decision: a clone target is admitted like a deallocate, because it removes
// as deallocate does and splits a held extent the same way. The RFC names only
// release and deallocate. Revisit if a clone target must never fail at the
// bound.
//
// decision: the reservation is adds, not the removal's own entry change. A
// removal also drops the pieces it covers, so the count can end below the
// ceiling even while splitting ones are refused; reserving only what the
// removal may add is what keeps a trim pass moving under the ceiling, and
// the commit's true delta keeps the count exact.
func (fs *files) admitRemoval(id core.FileID, e core.Extent, adds int64) (int64, error) {
	split := false
	if !fs.view(id, func(f *file) { split = f.splits(e) }) {
		return 0, nil
	}
	if !split {
		return 0, nil
	}
	for {
		c := fs.count.Load()
		limit := fs.limit.Load()
		// Saturate: a limit near MaxInt64 must not wrap the ceiling
		// negative, or every splitting removal would be refused where
		// nothing else is — content writes are still admitted against
		// the limit. A negative one, which the settings must reject
		// before the index, gives a negative ceiling and refuses the
		// same way it refuses everything else.
		eighth, room := limit/8, math.MaxInt64-limit
		if limit >= 0 && eighth > room {
			eighth = room
		}
		ceiling := limit + eighth
		if c+adds > ceiling {
			fs.refusals.Add(1)
			return 0, &core.NoSpaceError{Limit: core.LimitIndex, Missing: c + adds - ceiling, Unit: core.UnitEntries}
		}
		if fs.count.CompareAndSwap(c, c+adds) {
			return adds, nil
		}
	}
}

// abandon returns an admission the caller will never commit: the write's
// record was not written, so its entries never appear.
func (fs *files) abandon(admitted int64) {
	fs.count.Add(-admitted)
}
