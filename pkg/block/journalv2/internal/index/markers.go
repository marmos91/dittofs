package index

import (
	"cmp"
	"math"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// RemovalKind is the removal that left a marker. The zero RemovalKind names
// none: a Change with it is held content.
type RemovalKind uint8

const (
	// RemovalTruncate is a Truncate's marker, from the new size to the end of
	// the file.
	RemovalTruncate RemovalKind = iota + 1
	// RemovalDeallocate is a Deallocate's marker, over the punched extent.
	RemovalDeallocate
	// RemovalDelete is a Delete's marker, over the whole file.
	RemovalDelete
	// RemovalCloneTarget is a CloneTarget's marker, over the destination
	// extent; it keeps the clone's spec.
	RemovalCloneTarget
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
	kind    RemovalKind
	clone   *core.CloneSpec // a clone target's spec; nil for every other kind
}

// toEnd returns the extent from off to the end of the file.
func toEnd(off int64) core.Extent { return core.Extent{Off: off, Len: math.MaxInt64 - off} }

// end returns the offset just past e.
func end(e core.Extent) int64 { return e.Off + e.Len }

// truncate stops holding everything at or beyond size below v, narrowing a
// piece that straddles it, and leaves a marker from size to the end of the
// file.
func (f *file) truncate(size int64, v core.Version) {
	f.remove(marker{extent: toEnd(size), version: v, kind: RemovalTruncate})
}

// deallocate stops holding e below v and leaves a marker over it. A
// zero-length e is refused with ErrZeroLength and changes nothing: its
// record's length 0 would read as a removal to the end of the file.
func (f *file) deallocate(e core.Extent, v core.Version) error {
	if e.Len == 0 {
		return core.ErrZeroLength
	}
	f.remove(marker{extent: e, version: v, kind: RemovalDeallocate})
	return nil
}

// delete stops holding every extent of the file below v and leaves a marker
// over the whole file.
func (f *file) delete(v core.Version) {
	f.remove(marker{extent: toEnd(0), version: v, kind: RemovalDelete})
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
		kind:    RemovalCloneTarget,
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

// fill holds p as a fill does, where nothing is held, except inside a marker
// above p's version: a removal newer than the fetched content excludes it,
// so a cold read resolved against a ref older than the removal cannot bring
// the removed bytes back. A marker at p's version masks nothing, since only
// content below a marker's version is excluded.
func (f *file) fill(p Placed) {
	above := f.markers[f.firstAbove(p.Version):]
	if len(above) == 0 {
		f.held.fill(p)
		return
	}
	var masks []core.Extent
	for _, m := range above {
		if lo, hi := max(m.extent.Off, p.Extent.Off), min(end(m.extent), end(p.Extent)); lo < hi {
			masks = append(masks, core.Extent{Off: lo, Len: hi - lo})
		}
	}
	slices.SortFunc(masks, func(a, b core.Extent) int { return cmp.Compare(a.Off, b.Off) })
	part := func(lo, hi int64) {
		f.held.fill(Placed{
			Extent:  core.Extent{Off: lo, Len: hi - lo},
			Record:  p.Record,
			Skip:    p.Skip + uint32(lo-p.Extent.Off),
			Version: p.Version,
		})
	}
	cursor := p.Extent.Off
	for _, e := range masks {
		if e.Off > cursor {
			part(cursor, e.Off)
		}
		cursor = max(cursor, end(e))
	}
	if pend := end(p.Extent); pend > cursor {
		part(cursor, pend)
	}
}

// settleRemovals drops the markers at or below v: the caller's statement that
// metadata has recorded every removal of the file up to v. It appends nothing
// and is not persisted.
func (f *file) settleRemovals(v core.Version) {
	f.markers = slices.Delete(f.markers, 0, f.firstAbove(v))
}

// settleRemovals settles id's markers at or below v. It claims nothing, and
// leaves no entry for a file that had none.
func (fs *files) settleRemovals(id core.FileID, v core.Version) {
	_ = fs.update(id, func(f *file) error {
		f.settleRemovals(v)
		return nil
	})
}
