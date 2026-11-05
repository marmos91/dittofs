package model

import (
	"cmp"
	"context"
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// modelView serves a model's current state through the View a journal
// handle offers, so a second model can stand in for the journal Compare
// reads. It is the honest journal the planted views below corrupt.
type modelView struct {
	m   *Model
	tag journal.ShareTag
}

var _ View = modelView{}

func (v modelView) state(id journal.FileID) *fileState {
	if f := v.m.files[id]; f == nil || f.tag != v.tag {
		return &fileState{held: map[int64]cell{}, bit: map[int64]bool{}}
	}
	return v.m.current(id)
}

func (v modelView) ReadAt(_ context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	st := v.state(id)
	var missing []journal.Extent
	var held []journal.Held
	for i := range p {
		at := off + int64(i)
		c, ok := st.held[at]
		if !ok {
			if n := len(missing); n > 0 && end(missing[n-1]) == at {
				missing[n-1].Len++
			} else {
				missing = append(missing, ext(at, 1))
			}
			continue
		}
		p[i] = c.b
		if n := len(held); n > 0 && end(held[n-1].Extent) == at && held[n-1].Version == c.rec.ver {
			held[n-1].Extent.Len++
		} else {
			held = append(held, journal.Held{Extent: ext(at, 1), Version: c.rec.ver})
		}
	}
	return len(p), missing, held, st.change, nil
}

func (v modelView) Extents(id journal.FileID) ([]journal.Extent, error) {
	var out []journal.Extent
	for _, r := range v.state(id).runs() {
		if n := len(out); n > 0 && end(out[n-1]) == r.ext.Off {
			out[n-1].Len += r.ext.Len
			continue
		}
		out = append(out, r.ext)
	}
	return out, nil
}

func (v modelView) Since(_ context.Context, id journal.FileID, from journal.Version) iter.Seq2[journal.Change, error] {
	return func(yield func(journal.Change, error) bool) {
		cs, err := v.since(id, from)
		if err != nil {
			yield(journal.Change{}, err)
			return
		}
		for _, c := range cs {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func (v modelView) Files() iter.Seq[journal.FileID] {
	return func(yield func(journal.FileID) bool) {
		for id, f := range v.m.files {
			st := v.state(id)
			if f.tag == v.tag && (len(st.held) > 0 || len(st.markers) > 0) && !yield(id) {
				return
			}
		}
	}
}

func (v modelView) DirtyFiles() iter.Seq2[journal.FileID, time.Time] {
	return func(yield func(journal.FileID, time.Time) bool) {
		for id := range v.Files() {
			st := v.state(id)
			if slices.ContainsFunc(slices.Collect(maps.Keys(st.held)), func(off int64) bool { return !st.bit[off] }) && !yield(id, time.Time{}) {
				return
			}
		}
	}
}

func (v modelView) Index(id journal.FileID) ([]journal.IndexEntry, error) {
	st := v.state(id)
	var out []journal.IndexEntry
	for _, r := range st.runs() {
		out = append(out, journal.IndexEntry{Kind: journal.EntryHeld, Extent: r.ext, Version: r.ver, Offloaded: r.offloaded})
	}
	for _, r := range st.markers {
		out = append(out, journal.IndexEntry{Kind: journal.EntryMarker, Extent: r.ext, Version: r.ver})
	}
	return out, nil
}

// since answers Since(id, from) as the journal must: every held extent, as a
// write, and every live marker whose version is strictly above from, in
// version order. Held extents are split where the record they are read from
// changes; Compare checks Since byte by byte, so a journal may split them
// differently.
func (v modelView) since(id journal.FileID, from journal.Version) ([]journal.Change, error) {
	f := v.m.files[id]
	switch {
	case f == nil:
		return nil, nil
	case f.tag != v.tag:
		return nil, journal.ErrOtherShare
	}
	st := derive(f, all)
	var out []journal.Change
	for _, r := range st.runs() {
		if r.ver.Compare(from) <= 0 {
			continue
		}
		var mtime int64
		if r.rec.kind == kindWrite {
			mtime = r.rec.mtime
		}
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Version == r.ver && last.Mtime == mtime && last.Extent.Off+last.Extent.Len == r.ext.Off {
				last.Extent.Len += r.ext.Len
				continue
			}
		}
		out = append(out, journal.Change{Kind: journal.ChangeWrite, Extent: r.ext, Version: r.ver, Mtime: mtime})
	}
	for _, r := range st.markers {
		if r.ver.Compare(from) > 0 {
			out = append(out, journal.Change{Kind: changeKind(r.kind), Extent: r.ext, Version: r.ver})
		}
	}
	slices.SortStableFunc(out, func(a, b journal.Change) int {
		if c := a.Version.Compare(b.Version); c != 0 {
			return c
		}
		return cmp.Compare(a.Extent.Off, b.Extent.Off)
	})
	return out, nil
}
