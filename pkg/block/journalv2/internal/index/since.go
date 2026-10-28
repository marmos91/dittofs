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
// It copies under the file's read lock, as a read does, and sorts after
// releasing it, in O(n log n) for the n items yielded. The result is a copy,
// so working through it holds no lock. Loss records are the journal's side,
// not the index's.
func (fs *files) since(id core.FileID, from core.Version, out []Change) []Change {
	first := len(out)
	fs.view(id, func(f *file) {
		placed, _ := f.held.span(core.Extent{Off: 0, Len: math.MaxInt64}, nil, nil)
		for _, p := range placed {
			if p.Version.Compare(from) > 0 {
				out = append(out, Change{Extent: p.Extent, Version: p.Version, Record: p.Record})
			}
		}
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
