package index

import (
	"cmp"
	"math"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Change is one item of the index's side of Since: a held extent, with the
// record it is read from, or an unsettled removal marker, with its kind and,
// for a clone target, its spec. A held extent is a write whether it was
// written or filled. The record lets the caller read a write's mtime from its
// header, which the index does not keep.
type Change struct {
	Removal RemovalKind // zero for a held extent
	Extent  core.Extent // a held extent, or the extent a marker covers
	Version core.Version
	Record  RecordAt        // a held extent's record; zero for a marker
	Clone   *core.CloneSpec // a clone target's spec, a copy the caller may keep; nil otherwise
}

// since appends to out every held extent and every unsettled removal marker
// of id whose version is strictly above from, in version order, and returns
// it. A file with no entry yields nothing.
//
// Of one version, markers come before held extents, so a caller applying the
// changes in order never removes content held at a marker's own version,
// which the marker does not exclude; then extents come in offset order. A
// held extent is one record's contiguous piece: pieces of a record split only
// by their offloaded bit are one extent, since nothing yielded may depend on
// the bit.
//
// It walks the file's pieces under its read lock, as a read of the whole
// file does, copying only the items it yields, and sorts after releasing the
// lock, in O(n log n) for the n items yielded. The walk is O(N) for the N
// pieces held, since nothing orders them by version. The result is a copy,
// so working through it holds no lock. Loss records are the journal's side,
// not the index's.
func (fs *files) since(id core.FileID, from core.Version, out []Change) []Change {
	first := len(out)
	fs.view(id, func(f *file) {
		var lastSkip uint32 // where the last held item starts in its record
		f.held.visit(core.Extent{Off: 0, Len: math.MaxInt64}, func(p Piece) bool {
			if p.Version.Compare(from) <= 0 {
				return true
			}
			// A piece continuing the last one's record, split from it only by
			// its offloaded bit, extends it. The version test, as in joins,
			// only guards against a caller that names one record with two.
			if n := len(out); n > first {
				if last := &out[n-1]; end(last.Extent) == p.Extent.Off && last.Record == p.Record &&
					last.Version == p.Version && uint64(lastSkip)+uint64(last.Extent.Len) == uint64(p.Skip) {
					last.Extent.Len += p.Extent.Len
					return true
				}
			}
			out = append(out, Change{Extent: p.Extent, Version: p.Version, Record: p.Record})
			lastSkip = p.Skip
			return true
		})
		for _, m := range f.markers[f.firstAbove(from):] {
			c := Change{Removal: m.kind, Extent: m.extent, Version: m.version}
			if m.clone != nil {
				spec := *m.clone
				c.Clone = &spec
			}
			out = append(out, c)
		}
	})
	slices.SortFunc(out[first:], func(a, b Change) int {
		if c := a.Version.Compare(b.Version); c != 0 {
			return c
		}
		if a.Removal != 0 && b.Removal == 0 {
			return -1
		}
		if a.Removal == 0 && b.Removal != 0 {
			return 1
		}
		return cmp.Compare(a.Extent.Off, b.Extent.Off)
	})
	return out
}
