package index

import (
	"cmp"
	"math"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// marker is a removal marker: the extent a removal covers and the removal's
// version, holding no bytes and naming no record. Inside its extent, content
// with a version below the marker's is not held, and content at or above it
// is. A removal that runs to the end of the file, a truncate's or a delete's,
// has an extent ending at MaxInt64, never its record's length 0.
//
// A marker lives until the caller settles it at or above its version. Settling
// is not persisted: a rebuild makes every marker again from its removal
// record, unsettled.
type marker struct {
	extent  core.Extent
	version core.Version
	kind    core.ChangeKind // the removal that left it
	clone   *core.CloneSpec // a clone target's spec; nil for every other kind
}

// toEnd returns the extent from off to the end of the file.
func toEnd(off int64) core.Extent { return core.Extent{Off: off, Len: math.MaxInt64 - off} }

// extentEnd returns the offset just past e.
func extentEnd(e core.Extent) int64 { return e.Off + e.Len }

// truncate stops holding everything at or beyond size below v, narrowing a
// piece that straddles it, and leaves a marker from size to the end of the
// file.
func (f *file) truncate(size int64, v core.Version) {
	f.remove(marker{extent: toEnd(size), version: v, kind: core.ChangeTruncate})
}

// deallocate stops holding e below v and leaves a marker over it. A
// zero-length e is refused with ErrZeroLength and changes nothing: its
// record's length 0 would read as a removal to the end of the file.
func (f *file) deallocate(e core.Extent, v core.Version) error {
	if e.Len == 0 {
		return core.ErrZeroLength
	}
	f.remove(marker{extent: e, version: v, kind: core.ChangeDeallocate})
	return nil
}

// delete stops holding every extent of the file below v and leaves a marker
// over the whole file.
func (f *file) delete(v core.Version) {
	f.remove(marker{extent: toEnd(0), version: v, kind: core.ChangeDelete})
}

// cloneTarget stops holding (spec.DstOff, spec.Len) below v as deallocate
// does, and leaves a marker keeping its own copy of spec, so a restart that
// learns of it from Since resumes the clone rather than applying a plain
// removal. A zero-length spec is refused with ErrZeroLength and changes
// nothing.
func (f *file) cloneTarget(spec core.CloneSpec, v core.Version) error {
	if spec.Len == 0 {
		return core.ErrZeroLength
	}
	f.remove(marker{
		extent:  core.Extent{Off: spec.DstOff, Len: spec.Len},
		version: v,
		kind:    core.ChangeCloneTarget,
		clone:   &spec,
	})
	return nil
}

// remove applies m: content below its version inside its extent stops being
// held, whatever its offloaded bit, the marker is kept, and the change
// version is raised to m's, since a removal is versioned like a write.
//
// Markers are kept in version order, ties in arrival order. At run time a
// removal draws the newest version, so it is appended; a rebuild applies
// records in any order and inserts.
//
// ponytail: a file's markers are a slice in version order, so inserting out
// of order costs O(m), and a fill scans the markers above its version, O(m)
// for m unsettled markers of the file. Markers live only until the engine
// settles them, so m is small unless settling stalls. Upgrade to an interval
// structure keyed on offset when a fill's scan shows in a profile, as a trim
// pass of many punches left unsettled would make it.
//
// decision: removal offsets and lengths are trusted, as the held set's are:
// an offset is not negative and an extent's end does not overflow. A removal
// outside that range would store a marker whose end wraps negative, and a
// fill would then not be masked by it. Refusing such a range is the journal
// API's job, at the call that takes it from the caller, as it is for writes.
// A truncate to MaxInt64 removes nothing and keeps a marker of length 0,
// which Since still yields so metadata learns the size.
func (f *file) remove(m marker) {
	f.held.cutBelow(m.extent, m.version)
	f.markers = slices.Insert(f.markers, f.firstAbove(m.version), m)
	if m.version.Compare(f.change) > 0 {
		f.change = m.version
	}
}

// firstAbove returns the index of the file's first marker whose version is
// above v, or len(f.markers) when none is.
func (f *file) firstAbove(v core.Version) int {
	i, _ := slices.BinarySearchFunc(f.markers, v, func(x marker, v core.Version) int {
		if x.version.Compare(v) > 0 {
			return 1
		}
		return -1
	})
	return i
}

// fillParts calls fn on each part of p a fill would hold: the sub-extents of
// p's extent no marker above p's version masks, as a fill of p would hold
// them, in offset order. One helper reads the parts for both the fill itself
// and the fill's entry pre-check, so the two cannot diverge.
func (f *file) fillParts(p Placed, fn func(Placed)) {
	above := f.markers[f.firstAbove(p.Version):]
	if len(above) == 0 {
		fn(p)
		return
	}
	var masks []core.Extent
	for _, m := range above {
		if lo, hi := max(m.extent.Off, p.Extent.Off), min(extentEnd(m.extent), extentEnd(p.Extent)); lo < hi {
			masks = append(masks, core.Extent{Off: lo, Len: hi - lo})
		}
	}
	slices.SortFunc(masks, func(a, b core.Extent) int { return cmp.Compare(a.Off, b.Off) })
	cursor := p.Extent.Off
	part := func(lo, hi int64) {
		if lo >= hi {
			return
		}
		fn(Placed{
			Extent:  core.Extent{Off: lo, Len: hi - lo},
			Record:  p.Record,
			Skip:    p.Skip + uint32(lo-p.Extent.Off),
			Version: p.Version,
		})
	}
	for _, e := range masks {
		part(cursor, e.Off)
		cursor = max(cursor, extentEnd(e))
	}
	part(cursor, extentEnd(p.Extent))
}

// fill holds p as a fill does, where nothing is held, except inside a marker
// above p's version: a removal newer than the fetched content excludes it,
// so while the marker is unsettled a cold read resolved against a ref older
// than the removal cannot bring the removed bytes back. A marker at p's
// version masks nothing, since only content below a marker's version is
// excluded.
//
// decision: only unsettled markers mask a fill. Once a removal is settled its
// marker is gone, and a fill resolved before the removal is no longer masked
// here: refusing it is the fill staleness check's job, against the file's
// change version, which a removal raises. That check is not built yet, so
// until it is, a stale fill landing after Settle is held.
func (f *file) fill(p Placed) {
	f.fillParts(p, f.held.fill)
}

// settleRemovals drops the markers at or below v: the caller's statement that
// metadata has recorded every removal of the file up to v. It appends nothing
// and is not persisted.
func (f *file) settleRemovals(v core.Version) {
	f.markers = slices.Delete(f.markers, 0, f.firstAbove(v))
}

// settleRemovals settles id's markers at or below v. It claims nothing, and
// a file with no entry is left alone rather than given one to drop: Settle
// follows every metadata commit of a removal. A file that gains an entry
// between the two steps is settled under its lock like any other.
func (fs *files) settleRemovals(id core.FileID, v core.Version) {
	if !fs.view(id, func(*file) {}) {
		return
	}
	_ = fs.update(id, func(f *file) error {
		f.settleRemovals(v)
		return nil
	})
}
