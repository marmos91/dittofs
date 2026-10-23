package model

import (
	"context"
	"iter"
	"math"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// script drives the same calls into the model under test and into a second
// model playing the journal, so the two agree until a test plants a
// difference.
func script(t *testing.T, ms ...*Model) {
	t.Helper()
	for _, m := range ms {
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 20), 11, ver(1), nil))
		ok(t, m.WriteAt(shareA, fid(1), 30, fill('B', 10), 22, ver(2), nil))
		ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))))
		ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
		ok(t, m.EndOffer(shareA, fid(1), journalv2.Version{}, nil, nil))
		ok(t, m.Truncate(shareA, fid(1), 35, ver(3), nil))
		ok(t, m.WriteAt(shareB, fid(2), 100, fill('C', 5), 33, ver(4), nil))
		ok(t, m.Delete(shareB, fid(2), ver(5), nil))
		ok(t, m.WriteAt(shareB, fid(2), 0, fill('D', 5), 44, ver(6), nil))
	}
}

// Compare passes when the journal shows what the model holds, share by share.
func TestCompareAcceptsTheSameState(t *testing.T) {
	m, j := New(), New()
	script(t, m, j)
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
	ok(t, m.Compare(t.Context(), shareB, modelView{j, shareB}))
}

// planted wraps an honest view and changes one answer.
type planted struct {
	modelView
	readAt  func(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error)
	extents func(id journalv2.FileID) ([]journalv2.Extent, error)
	since   func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error]
	index   func(id journalv2.FileID) ([]journalv2.IndexEntry, error)
	files   func() iter.Seq[journalv2.FileID]
	dirty   func() iter.Seq2[journalv2.FileID, time.Time]
}

func (p planted) DirtyFiles() iter.Seq2[journalv2.FileID, time.Time] {
	if p.dirty != nil {
		return p.dirty()
	}
	return p.modelView.DirtyFiles()
}

func (p planted) ReadAt(ctx context.Context, id journalv2.FileID, off int64, b []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	if p.readAt != nil {
		return p.readAt(id, off, b)
	}
	return p.modelView.ReadAt(ctx, id, off, b)
}

func (p planted) Extents(id journalv2.FileID) ([]journalv2.Extent, error) {
	if p.extents != nil {
		return p.extents(id)
	}
	return p.modelView.Extents(id)
}

func (p planted) Since(ctx context.Context, id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
	if p.since != nil {
		return p.since(id, from)
	}
	return p.modelView.Since(ctx, id, from)
}

func (p planted) Index(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
	if p.index != nil {
		return p.index(id)
	}
	return p.modelView.Index(id)
}

func (p planted) Files() iter.Seq[journalv2.FileID] {
	if p.files != nil {
		return p.files()
	}
	return p.modelView.Files()
}

func changes(cs []journalv2.Change) iter.Seq2[journalv2.Change, error] {
	return func(yield func(journalv2.Change, error) bool) {
		for _, c := range cs {
			if !yield(c, nil) {
				return
			}
		}
	}
}

// Each planted divergence is reported: Compare can fail on every answer it
// reads.
func TestCompareReportsEachPlantedDivergence(t *testing.T) {
	cases := []struct {
		name  string
		plant func(h modelView) View
	}{
		{"one wrong byte", func(h modelView) View {
			return planted{modelView: h, readAt: func(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
				n, mi, he, as, err := h.ReadAt(t.Context(), id, off, p)
				if off <= 5 && 5 < off+int64(len(p)) {
					p[5-off] ^= 0xff
				}
				return n, mi, he, as, err
			}}
		}},
		{"missing widened over a held byte", func(h modelView) View {
			return planted{modelView: h, readAt: func(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
				n, _, _, as, err := h.ReadAt(t.Context(), id, off, p)
				return n, []journalv2.Extent{ext(off, int64(len(p)))}, nil, as, err
			}}
		}},
		{"a held byte at the wrong version", func(h modelView) View {
			return planted{modelView: h, readAt: func(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
				n, mi, he, as, err := h.ReadAt(t.Context(), id, off, p)
				for i := range he {
					he[i].Version = ver(99)
				}
				return n, mi, he, as, err
			}}
		}},
		{"asOf older than the file", func(h modelView) View {
			return planted{modelView: h, readAt: func(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
				n, mi, he, _, err := h.ReadAt(t.Context(), id, off, p)
				return n, mi, he, ver(1), err
			}}
		}},
		{"an extent not held", func(h modelView) View {
			return planted{modelView: h, extents: func(id journalv2.FileID) ([]journalv2.Extent, error) {
				es, err := h.Extents(id)
				return append(es, ext(1000, 1)), err
			}}
		}},
		{"an offloaded bit set on dirty content", func(h modelView) View {
			return planted{modelView: h, index: func(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
				es, err := h.Index(id)
				for i := range es {
					es[i].Offloaded = true
				}
				return es, err
			}}
		}},
		{"a marker missing from Since", func(h modelView) View {
			return planted{modelView: h, since: func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
				var cs []journalv2.Change
				for c := range h.Since(t.Context(), id, from) {
					if c.Kind == journalv2.ChangeWrite {
						cs = append(cs, c)
					}
				}
				return changes(cs)
			}}
		}},
		{"a write's mtime in Since", func(h modelView) View {
			return planted{modelView: h, since: func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
				var cs []journalv2.Change
				for c := range h.Since(t.Context(), id, from) {
					c.Mtime++
					cs = append(cs, c)
				}
				return changes(cs)
			}}
		}},
		{"Since out of version order", func(h modelView) View {
			return planted{modelView: h, since: func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
				var cs []journalv2.Change
				for c := range h.Since(t.Context(), id, from) {
					cs = append([]journalv2.Change{c}, cs...)
				}
				return changes(cs)
			}}
		}},
		{"Extents omits a held byte", func(h modelView) View {
			return planted{modelView: h, extents: func(id journalv2.FileID) ([]journalv2.Extent, error) {
				es, err := h.Extents(id)
				if len(es) > 0 {
					es[0].Off++
					es[0].Len--
				}
				return es, err
			}}
		}},
		{"Since names a byte not held", func(h modelView) View {
			return planted{modelView: h, since: func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
				cs := []journalv2.Change{{Kind: journalv2.ChangeWrite, Extent: ext(1000, 1), Version: ver(1)}}
				for c := range h.Since(t.Context(), id, from) {
					cs = append(cs, c)
				}
				return changes(cs)
			}}
		}},
		{"the index holds a byte not held", func(h modelView) View {
			return planted{modelView: h, index: func(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
				es, err := h.Index(id)
				return append(es, journalv2.IndexEntry{Kind: journalv2.EntryHeld, Extent: ext(1000, 1), Version: ver(1)}), err
			}}
		}},
		{"the index serves a held byte at the wrong version", func(h modelView) View {
			return planted{modelView: h, index: func(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
				es, err := h.Index(id)
				for i := range es {
					if es[i].Kind == journalv2.EntryHeld {
						es[i].Version = ver(99)
					}
				}
				return es, err
			}}
		}},
		{"the index omits a marker", func(h modelView) View {
			return planted{modelView: h, index: func(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
				es, err := h.Index(id)
				var held []journalv2.IndexEntry
				for _, e := range es {
					if e.Kind == journalv2.EntryHeld {
						held = append(held, e)
					}
				}
				return held, err
			}}
		}},
		{"DirtyFiles omits a dirty file", func(h modelView) View {
			return planted{modelView: h, dirty: func() iter.Seq2[journalv2.FileID, time.Time] {
				return func(func(journalv2.FileID, time.Time) bool) {}
			}}
		}},
		{"asOf above every version issued", func(h modelView) View {
			return planted{modelView: h, readAt: func(id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
				n, mi, he, _, err := h.ReadAt(t.Context(), id, off, p)
				return n, mi, he, ver(1000), err
			}}
		}},
		{"a file the share does not hold", func(h modelView) View {
			return planted{modelView: h, files: func() iter.Seq[journalv2.FileID] {
				return func(yield func(journalv2.FileID) bool) {
					for id := range h.Files() {
						if !yield(id) {
							return
						}
					}
					yield(fid(9))
				}
			}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, j := New(), New()
			script(t, m, j)
			diverged(t, m.Compare(t.Context(), shareA, c.plant(modelView{j, shareA})))
		})
	}
}

// A marker of a truncate or a delete runs to the end of the file, and Since
// and the index say so with a length reaching the largest offset; a length of
// zero is not that form.
func TestCompareReadsAToEndMarkerInOneForm(t *testing.T) {
	withLen := func(view modelView, n int64) View {
		return planted{modelView: view,
			since: func(id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
				var cs []journalv2.Change
				for c := range view.Since(t.Context(), id, from) {
					if c.Kind == journalv2.ChangeTruncate {
						c.Extent.Len = n
					}
					cs = append(cs, c)
				}
				return changes(cs)
			},
			index: func(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
				es, err := view.Index(id)
				for i := range es {
					if es[i].Kind == journalv2.EntryMarker {
						es[i].Extent.Len = n
					}
				}
				return es, err
			},
		}
	}
	m, j := New(), New()
	script(t, m, j)
	ok(t, m.Compare(t.Context(), shareA, withLen(modelView{j, shareA}, math.MaxInt64-35)))
	ok(t, m.Compare(t.Context(), shareA, withLen(modelView{j, shareA}, math.MaxInt64)))
	diverged(t, m.Compare(t.Context(), shareA, withLen(modelView{j, shareA}, 0)))
}
