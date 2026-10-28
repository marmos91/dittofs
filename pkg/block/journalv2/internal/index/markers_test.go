package index

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// change runs fn on id's entry under the file's write lock and fails the test
// on an error.
func change(t *testing.T, fs *files, id core.FileID, fn func(*file) error) {
	t.Helper()
	if err := fs.update(id, fn); err != nil {
		t.Fatalf("update: %v", err)
	}
}

// heldPieces returns id's held pieces, or nil when the file has no entry.
func heldPieces(fs *files, id core.FileID) []Piece {
	var ps []Piece
	fs.view(id, func(f *file) { ps = f.held.pieces(nil) })
	return ps
}

// markerCount returns how many removal markers id keeps, 0 when it has no
// entry.
func markerCount(fs *files, id core.FileID) int {
	n := 0
	fs.view(id, func(f *file) { n = len(f.markers) })
	return n
}

// removals are the four removals, each over [4, 12) of a file or, for the two
// that run to the end, from 4 or from 0.
var removals = []struct {
	name   string
	kind   RemovalKind
	extent core.Extent
	apply  func(f *file, v core.Version) error
}{
	{"truncate", RemovalTruncate, ext(4, math.MaxInt64-4), func(f *file, v core.Version) error { f.truncate(4, v); return nil }},
	{"deallocate", RemovalDeallocate, ext(4, 8), func(f *file, v core.Version) error { return f.deallocate(ext(4, 8), v) }},
	{"delete", RemovalDelete, ext(0, math.MaxInt64-0), func(f *file, v core.Version) error { f.delete(v); return nil }},
	{"clone target", RemovalCloneTarget, ext(4, 8), func(f *file, v core.Version) error {
		return f.cloneTarget(core.CloneSpec{Src: fid(9), SrcOff: 100, DstOff: 4, Len: 8, AsOf: ver(1)}, v)
	}},
}

// TestRemovalIsNotGatedOnTheOffloadedBit checks that no removal is gated on
// the offloaded bit. Each removal runs over a dirty piece, an offloaded
// piece and a record split between the two, and must drop every byte of them
// inside its extent. A removal that kept dirty content, as a release does,
// would let an un-offloaded delete come back after a crash and resurrect a
// removed file.
func TestRemovalIsNotGatedOnTheOffloadedBit(t *testing.T) {
	for _, r := range removals {
		for _, state := range []string{"dirty", "offloaded", "partly offloaded"} {
			t.Run(r.name+"/"+state, func(t *testing.T) {
				var fs files
				id := fid(1)
				change(t, &fs, id, func(f *file) error {
					f.write(write(4, 8, rec(1, 0), 2))
					switch state {
					case "offloaded":
						f.held.markOffloaded(ext(4, 8), ver(2), ver(2))
					case "partly offloaded":
						f.held.markOffloaded(ext(4, 4), ver(2), ver(2))
					}
					return r.apply(f, ver(3))
				})
				if ps := heldPieces(&fs, id); len(ps) != 0 {
					t.Fatalf("held after %s over %s content: %+v, want nothing", r.name, state, ps)
				}
			})
		}
	}
}

// TestRemovalLeavesAMarker checks that each removal leaves a marker with its
// extent and version and no location, until settled. A to-end removal's
// extent is kept as running to MaxInt64, never as its record's length 0,
// which would read as a removal of nothing.
func TestRemovalLeavesAMarker(t *testing.T) {
	for _, r := range removals {
		t.Run(r.name, func(t *testing.T) {
			var fs files
			id := fid(1)
			change(t, &fs, id, func(f *file) error { return r.apply(f, ver(5)) })
			var ms []marker
			fs.view(id, func(f *file) { ms = slices.Clone(f.markers) })
			if len(ms) != 1 {
				t.Fatalf("markers = %+v, want one", ms)
			}
			m := ms[0]
			if m.kind != r.kind || m.extent != r.extent || m.version != ver(5) {
				t.Fatalf("marker = %+v, want kind %d, extent %+v, version 5", m, r.kind, r.extent)
			}
			if (m.clone != nil) != (r.kind == RemovalCloneTarget) {
				t.Fatalf("marker clone spec = %+v for kind %d", m.clone, r.kind)
			}
		})
	}
}

// TestRemovalKeepsContentAtOrAboveItsVersion is the marker's rule: inside its
// extent, content below its version is not held and content at or above it
// is. At run time a removal's version is above every
// version held, but a rebuild applies records in any order, so a removal can
// arrive after a fill at its own version or a write above it. A removal that
// dropped everything in its extent would drop them, and the result would
// depend on the order the records were read.
func TestRemovalKeepsContentAtOrAboveItsVersion(t *testing.T) {
	for _, r := range removals {
		t.Run(r.name, func(t *testing.T) {
			var fs files
			id := fid(1)
			below, at, above := rec(1, 0), rec(2, 0), rec(3, 0)
			change(t, &fs, id, func(f *file) error {
				f.held.insert(write(4, 2, below, 2))
				f.held.fill(write(6, 2, at, 5))
				f.held.insert(write(8, 4, above, 7))
				return r.apply(f, ver(5))
			})
			want := []Piece{
				{Extent: ext(6, 2), Record: at, Version: ver(5), Offloaded: true},
				{Extent: ext(8, 4), Record: above, Version: ver(7)},
			}
			if got := heldPieces(&fs, id); !slices.Equal(got, want) {
				t.Fatalf("held:\n got  %+v\n want %+v", got, want)
			}
		})
	}
}

// TestZeroLengthRemovalIsRefused checks that a Deallocate or CloneTarget of a
// zero-length extent is refused and changes nothing. Its record's
// length 0 would read as a removal to the end of the file, so accepting it
// would, after a restart, remove everything from its offset on.
func TestZeroLengthRemovalIsRefused(t *testing.T) {
	for name, apply := range map[string]func(*file) error{
		"deallocate":   func(f *file) error { return f.deallocate(ext(4, 0), ver(3)) },
		"clone target": func(f *file) error { return f.cloneTarget(core.CloneSpec{Src: fid(9), DstOff: 4, Len: 0}, ver(3)) },
	} {
		t.Run(name, func(t *testing.T) {
			var fs files
			id := fid(1)
			change(t, &fs, id, func(f *file) error { f.write(write(0, 16, rec(1, 0), 2)); return nil })
			err := fs.update(id, apply)
			if !errors.Is(err, core.ErrZeroLength) {
				t.Fatalf("err = %v, want ErrZeroLength", err)
			}
			fs.view(id, func(f *file) {
				if len(f.markers) != 0 {
					t.Fatalf("markers = %+v after a refused removal", f.markers)
				}
				if v, _ := f.changeVersion(); v != ver(2) {
					t.Fatalf("change version = %v after a refused removal, want 2", v)
				}
			})
			if ps := heldPieces(&fs, id); len(ps) != 1 || ps[0].Extent != ext(0, 16) {
				t.Fatalf("held = %+v after a refused removal, want (0, 16) untouched", ps)
			}
		})
	}
}

// TestRemovalRaisesTheChangeVersion checks that each removal is versioned
// like a write, so it raises the file's change version. One that did not
// would let a fill resolved before the removal pass the staleness check.
func TestRemovalRaisesTheChangeVersion(t *testing.T) {
	for _, r := range removals {
		t.Run(r.name, func(t *testing.T) {
			var fs files
			id := fid(1)
			change(t, &fs, id, func(f *file) error {
				f.write(write(0, 16, rec(1, 0), 2))
				return r.apply(f, ver(5))
			})
			fs.view(id, func(f *file) {
				if v, ok := f.changeVersion(); !ok || v != ver(5) {
					t.Fatalf("change version = %v, %v, want 5, true", v, ok)
				}
			})
		})
	}
}

// TestTruncateEdgeCases are the truncate edge cases: inside an extent, to
// zero and written again, and down and back up.
func TestTruncateEdgeCases(t *testing.T) {
	// Inside an extent: the extent straddling the size is narrowed, keeping
	// its head at the right place in its record, and nothing past it is held.
	t.Run("inside an extent", func(t *testing.T) {
		var fs files
		id := fid(1)
		a, b := rec(1, 0), rec(1, 64)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 8, a, 2))
			f.write(write(16, 8, b, 3))
			f.truncate(4, ver(4))
			return nil
		})
		want := []Piece{{Extent: ext(0, 4), Record: a, Version: ver(2)}}
		if got := heldPieces(&fs, id); !slices.Equal(got, want) {
			t.Fatalf("held:\n got  %+v\n want %+v", got, want)
		}
	})

	// To zero and write again: the new write is held although the marker of
	// the truncate below it still covers its extent. A marker that dropped
	// content whatever its version would hide every write after a truncate
	// until the truncate was settled.
	t.Run("to zero and write again", func(t *testing.T) {
		var fs files
		id := fid(1)
		a, b := rec(1, 0), rec(1, 64)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 8, a, 2))
			f.truncate(0, ver(3))
			f.write(write(0, 4, b, 4))
			return nil
		})
		want := []Piece{{Extent: ext(0, 4), Record: b, Version: ver(4)}}
		if got := heldPieces(&fs, id); !slices.Equal(got, want) {
			t.Fatalf("held:\n got  %+v\n want %+v", got, want)
		}
		if n := markerCount(&fs, id); n != 1 {
			t.Fatalf("markers = %d, want the truncate's", n)
		}
	})

	// Down and back up: the truncate up removes nothing, and what the
	// truncate down removed stays missing rather than reappearing.
	t.Run("down and back up", func(t *testing.T) {
		var fs files
		id := fid(1)
		a := rec(1, 0)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 16, a, 2))
			f.truncate(4, ver(3))
			f.truncate(16, ver(4))
			return nil
		})
		var placed []Placed
		var missing []core.Extent
		fs.view(id, func(f *file) { placed, missing = f.held.span(ext(0, 16), nil, nil) })
		if want := []Placed{{Extent: ext(0, 4), Record: a, Version: ver(2)}}; !slices.Equal(placed, want) {
			t.Fatalf("placed = %+v, want %+v", placed, want)
		}
		if want := []core.Extent{ext(4, 12)}; !slices.Equal(missing, want) {
			t.Fatalf("missing = %+v, want %+v", missing, want)
		}
		if n := markerCount(&fs, id); n != 2 {
			t.Fatalf("markers = %d, want both truncates'", n)
		}
	})
}

// TestDeallocateEdgeCases are the deallocate edge cases: inside one extent
// and across several.
func TestDeallocateEdgeCases(t *testing.T) {
	// Inside one extent: it splits in three, the tail keeping its place in
	// the record. A split that kept the head's skip for the tail would serve
	// the wrong bytes for it.
	t.Run("inside an extent", func(t *testing.T) {
		var fs files
		id := fid(1)
		a := rec(1, 0)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 12, a, 2))
			return f.deallocate(ext(4, 4), ver(3))
		})
		want := []Piece{
			{Extent: ext(0, 4), Record: a, Skip: 0, Version: ver(2)},
			{Extent: ext(8, 4), Record: a, Skip: 8, Version: ver(2)},
		}
		if got := heldPieces(&fs, id); !slices.Equal(got, want) {
			t.Fatalf("held:\n got  %+v\n want %+v", got, want)
		}
	})

	// Across extents: the two it crosses are narrowed and the ones inside it
	// dropped.
	t.Run("across extents", func(t *testing.T) {
		var fs files
		id := fid(1)
		a, b, c, d := rec(1, 0), rec(1, 64), rec(1, 128), rec(1, 192)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 8, a, 2))
			f.write(write(8, 4, b, 3))
			f.write(write(16, 4, c, 4))
			f.write(write(20, 8, d, 5))
			return f.deallocate(ext(4, 20), ver(6))
		})
		want := []Piece{
			{Extent: ext(0, 4), Record: a, Skip: 0, Version: ver(2)},
			{Extent: ext(24, 4), Record: d, Skip: 4, Version: ver(5)},
		}
		if got := heldPieces(&fs, id); !slices.Equal(got, want) {
			t.Fatalf("held:\n got  %+v\n want %+v", got, want)
		}
	})
}

// TestNoResurrection checks that a removal is never undone by an older fill,
// for each removal: write at v2, remove at v3, then fill the extent at v2, as a
// cold read resolving against a ref older than the removal would. The fill
// must hold nothing inside the marker, since content below its version is not
// held there. A fill at v3 is held, since only content below the marker's
// version is excluded. A fill that consulted only the held set would find
// the extent absent and bring the removed bytes back.
func TestNoResurrection(t *testing.T) {
	for _, r := range removals {
		t.Run(r.name, func(t *testing.T) {
			var fs files
			id := fid(1)
			change(t, &fs, id, func(f *file) error {
				f.write(write(4, 8, rec(1, 0), 2))
				if err := r.apply(f, ver(3)); err != nil {
					return err
				}
				f.fill(write(4, 8, rec(2, 0), 2))
				return nil
			})
			if ps := heldPieces(&fs, id); len(ps) != 0 {
				t.Fatalf("fill at v2 under a v3 marker held %+v, want nothing", ps)
			}

			filled := rec(3, 0)
			change(t, &fs, id, func(f *file) error { f.fill(write(4, 8, filled, 3)); return nil })
			want := []Piece{{Extent: ext(4, 8), Record: filled, Version: ver(3), Offloaded: true}}
			if got := heldPieces(&fs, id); !slices.Equal(got, want) {
				t.Fatalf("fill at v3 under a v3 marker:\n got  %+v\n want %+v", got, want)
			}
		})
	}
}

// TestFillHoldsOutsideTheMarker checks that a fill masked by a marker over
// part of its extent still holds the rest, each part at its own place in the
// fill's record. A fill dropped whole by any overlap would leave a cold read
// fetching the same bytes again, and one that kept the fill's skip for the
// tail would serve the head's bytes there.
func TestFillHoldsOutsideTheMarker(t *testing.T) {
	var fs files
	id := fid(1)
	r := rec(2, 0)
	change(t, &fs, id, func(f *file) error {
		// The newer marker lies at the lower offset, so the masks arrive out
		// of offset order: a fill walking them in version order would hold
		// [0, 10) at the first and bring back what the second removed.
		if err := f.deallocate(ext(10, 2), ver(3)); err != nil {
			return err
		}
		if err := f.deallocate(ext(4, 4), ver(5)); err != nil {
			return err
		}
		// The v1 marker below the fill masks nothing.
		f.truncate(0, ver(1))
		f.fill(Placed{Extent: ext(0, 16), Record: r, Skip: 32, Version: ver(2)})
		return nil
	})
	want := []Piece{
		{Extent: ext(0, 4), Record: r, Skip: 32, Version: ver(2), Offloaded: true},
		{Extent: ext(8, 2), Record: r, Skip: 40, Version: ver(2), Offloaded: true},
		{Extent: ext(12, 4), Record: r, Skip: 44, Version: ver(2), Offloaded: true},
	}
	if got := heldPieces(&fs, id); !slices.Equal(got, want) {
		t.Fatalf("held:\n got  %+v\n want %+v", got, want)
	}
}

// TestCloneSpecIsKept is the clone spec round trip at the index: the marker
// keeps the spec it was given, unchanged by anything the caller does to its
// copy afterwards, and the destination extent is not held. A marker that kept
// only the extent would hand a restart a plain removal, and the destination
// would read zeros.
func TestCloneSpecIsKept(t *testing.T) {
	var fs files
	id := fid(1)
	spec := core.CloneSpec{
		SrcShare: core.ShareID{0: 7},
		Src:      fid(9),
		SrcOff:   1 << 20,
		DstOff:   4,
		Len:      8,
		AsOf:     ver(11),
	}
	want := spec
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 16, rec(1, 0), 2))
		return f.cloneTarget(spec, ver(12))
	})
	spec.Src, spec.Len = fid(10), 99

	cs := fs.since(id, core.Version{}, nil)
	var got *core.CloneSpec
	for _, c := range cs {
		if c.Removal == RemovalCloneTarget {
			got = c.Clone
			if c.Extent != ext(4, 8) || c.Version != ver(12) {
				t.Fatalf("clone marker = %+v, want extent (4, 8) at v12", c)
			}
		}
	}
	if got == nil || *got != want {
		t.Fatalf("clone spec = %+v, want %+v", got, want)
	}
	// What Since hands out is a copy: a caller changing it changes no marker.
	got.Len = 1
	for _, c := range fs.since(id, core.Version{}, nil) {
		if c.Removal == RemovalCloneTarget && *c.Clone != want {
			t.Fatalf("clone spec = %+v after the caller changed Since's copy, want %+v", *c.Clone, want)
		}
	}
	fs.view(id, func(f *file) {
		if _, ok := f.held.lookup(6); ok {
			t.Fatalf("destination offset 6 still held after the clone target")
		}
	})
}

// TestMarkerKeepsTheEntry is the change-version lifetime with markers: a file
// holding only a marker keeps its entry and change version, and settling the
// marker drops both when no segment holds a record of the file. An entry
// dropped while a marker remained would lose the removal Since must still
// yield; one kept after would never be freed.
func TestMarkerKeepsTheEntry(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 8, rec(1, 0), 2))
		f.delete(ver(3))
		return nil
	})
	if !present(&fs, id) {
		t.Fatalf("entry dropped while a delete marker remains")
	}
	fs.view(id, func(f *file) {
		if v, ok := f.changeVersion(); !ok || v != ver(3) {
			t.Fatalf("change version = %v, %v, want 3, true", v, ok)
		}
	})

	fs.settleRemovals(id, ver(2))
	if n := markerCount(&fs, id); n != 1 {
		t.Fatalf("markers = %d after settling below the delete, want 1", n)
	}
	fs.settleRemovals(id, ver(3))
	if present(&fs, id) {
		t.Fatalf("entry kept after its last marker was settled and nothing is held")
	}
}

// TestSettleOfAnAbsentFileLeavesNoEntry checks that Settle claims and
// creates nothing: it is a statement about metadata, not a call on a file.
func TestSettleOfAnAbsentFileLeavesNoEntry(t *testing.T) {
	var fs files
	id := fid(1)
	fs.settleRemovals(id, ver(9))
	if present(&fs, id) || fs.len() != 0 {
		t.Fatalf("settle of an absent file left an entry")
	}
}

// TestSettleDropsOnlyMarkersAtOrBelow checks Settle against out-of-order
// markers, as a rebuild inserts them: settling drops every marker at or below
// v and only those, whatever order they arrived in. A settle that stopped at
// the first marker above v in arrival order would leave a settled one behind.
func TestSettleDropsOnlyMarkersAtOrBelow(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.truncate(8, ver(9))
		if err := f.deallocate(ext(0, 2), ver(3)); err != nil {
			return err
		}
		f.truncate(4, ver(6))
		return nil
	})
	fs.settleRemovals(id, ver(6))
	var vs []core.Version
	fs.view(id, func(f *file) {
		for _, m := range f.markers {
			vs = append(vs, m.version)
		}
	})
	if want := []core.Version{ver(9)}; !slices.Equal(vs, want) {
		t.Fatalf("markers left = %v, want %v", vs, want)
	}
}
