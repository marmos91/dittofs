package model

import (
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// modelView serves a model's current state through the View a journal
// handle offers, so a second model can stand in for the journal Compare
// reads. It is the honest journal the planted views below corrupt.
type modelView struct {
	m   *Model
	tag journalv2.ShareTag
}

var _ View = modelView{}

func (v modelView) state(id journalv2.FileID) *fileState {
	if f := v.m.files[id]; f == nil || f.tag != v.tag {
		return &fileState{held: map[int64]cell{}, bit: map[int64]bool{}}
	}
	return v.m.current(id)
}

func (v modelView) ReadAt(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	st := v.state(id)
	var missing []journalv2.Extent
	var held []journalv2.Held
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
			held = append(held, journalv2.Held{Extent: ext(at, 1), Version: c.rec.ver})
		}
	}
	return len(p), missing, held, st.change, nil
}

func (v modelView) Extents(id journalv2.FileID) ([]journalv2.Extent, error) {
	var out []journalv2.Extent
	for _, r := range v.state(id).runs() {
		if n := len(out); n > 0 && end(out[n-1]) == r.ext.Off {
			out[n-1].Len += r.ext.Len
			continue
		}
		out = append(out, r.ext)
	}
	return out, nil
}

func (v modelView) Since(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
	return func(yield func(journalv2.Change, error) bool) {
		for _, c := range v.m.Since(v.tag, id, from) {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func (v modelView) Files() iter.Seq[journalv2.FileID] {
	return func(yield func(journalv2.FileID) bool) {
		for id, f := range v.m.files {
			st := v.state(id)
			if f.tag == v.tag && (len(st.held) > 0 || len(st.markers) > 0) && !yield(id) {
				return
			}
		}
	}
}

func (v modelView) DirtyFiles() iter.Seq2[journalv2.FileID, time.Time] {
	return func(yield func(journalv2.FileID, time.Time) bool) {
		for id := range v.Files() {
			st := v.state(id)
			if slices.ContainsFunc(slices.Collect(maps.Keys(st.held)), func(off int64) bool { return !st.bit[off] }) && !yield(id, time.Time{}) {
				return
			}
		}
	}
}

func (v modelView) Index(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
	st := v.state(id)
	var out []journalv2.IndexEntry
	for _, r := range st.runs() {
		out = append(out, journalv2.IndexEntry{Kind: journalv2.EntryHeld, Extent: r.ext, Version: r.ver, Offloaded: r.offloaded})
	}
	for _, r := range st.markers {
		out = append(out, journalv2.IndexEntry{Kind: journalv2.EntryMarker, Extent: r.ext, Version: r.ver})
	}
	return out, nil
}
