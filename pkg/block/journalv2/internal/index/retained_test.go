package index

import (
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// piece is a held piece, for building expectations.
func piece(off, n int64, r RecordAt, skip uint32, v uint64, offloaded bool) Piece {
	return Piece{Extent: ext(off, n), Record: r, Skip: skip, Version: ver(v), Offloaded: offloaded}
}

// kept is a retained piece superseded by the record of version by.
func kept(p Piece, by uint64) Retained { return Retained{Piece: p, By: ver(by)} }

// assertRetained checks the file's retained read-out against want, in order.
func assertRetained(t *testing.T, f *file, want ...Retained) {
	t.Helper()
	got := f.retained.list(nil)
	if !slices.Equal(got, want) {
		t.Fatalf("retained:\n got  %+v\n want %+v", got, want)
	}
	if f.retained.len() != len(want) {
		t.Fatalf("retained len() = %d, want %d", f.retained.len(), len(want))
	}
}

// failed names one record of a failed window.
func failed(r RecordAt, v uint64) WindowRecord { return WindowRecord{Record: r, Version: ver(v)} }

// TestRetainedKeepsOverlappingPieces: retained pieces may overlap each other
// and the held set, and are listed in offset then version order. A set that
// kept them disjoint, as the held set is kept, would lose the older of two
// versions an offload still has to offer.
func TestRetainedKeepsOverlappingPieces(t *testing.T) {
	var f file
	f.write(write(0, 16, rec(1, 0), 1))
	f.writeRetain(write(4, 8, rec(2, 0), 2))
	f.writeRetain(write(0, 16, rec(3, 0), 3))
	assertPieces(t, &f.held, piece(0, 16, rec(3, 0), 0, 3, false))
	assertRetained(t, &f,
		kept(piece(0, 4, rec(1, 0), 0, 1, false), 3),
		kept(piece(4, 8, rec(1, 0), 4, 1, false), 2),
		kept(piece(4, 8, rec(2, 0), 0, 2, false), 3),
		kept(piece(12, 4, rec(1, 0), 12, 1, false), 3),
	)
}

// TestWriteRetainMovesSupersededPieces is the three-way split with retain: the
// middle of the old piece leaves the held set and is retained with its own
// record, skip and version, named as superseded by the new write; the head and
// tail stay held. A retain that kept the old piece's skip would offer the
// head's bytes as the superseded middle.
func TestWriteRetainMovesSupersededPieces(t *testing.T) {
	var f file
	f.write(write(0, 12, rec(1, 64), 1))
	f.writeRetain(write(4, 4, rec(2, 0), 2))
	assertPieces(t, &f.held,
		piece(0, 4, rec(1, 64), 0, 1, false),
		piece(4, 4, rec(2, 0), 0, 2, false),
		piece(8, 4, rec(1, 64), 8, 1, false),
	)
	assertRetained(t, &f, kept(piece(4, 4, rec(1, 64), 4, 1, false), 2))
	if v, _ := f.changeVersion(); v != ver(2) {
		t.Fatalf("change version = %v, want %v", v, ver(2))
	}
}

// TestWriteRetainOverSeveralPiecesAndAGap: a write over parts of two pieces
// and the gap between them retains exactly the two parts. A gap holds nothing,
// so there is nothing to keep there.
func TestWriteRetainOverSeveralPiecesAndAGap(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.write(write(16, 8, rec(2, 0), 2))
	f.writeRetain(write(4, 16, rec(3, 0), 3))
	assertRetained(t, &f,
		kept(piece(4, 4, rec(1, 0), 4, 1, false), 3),
		kept(piece(16, 4, rec(2, 0), 0, 2, false), 3),
	)
}

// TestWriteRetainKeepsOffloadedBit: a retained piece keeps the bit it had when
// held, so offload does not offer again what was committed, and a piece split
// by its bit is retained as two.
func TestWriteRetainKeepsOffloadedBit(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.held.markOffloaded(ext(0, 4), ver(1), ver(1))
	f.writeRetain(write(0, 8, rec(2, 0), 2))
	assertRetained(t, &f,
		kept(piece(0, 4, rec(1, 0), 0, 1, true), 2),
		kept(piece(4, 4, rec(1, 0), 4, 1, false), 2),
	)
}

// TestReadNeverServesRetained: a read is answered from held extents only.
// Once the newest write is cut, the read finds a gap there, not the retained
// version under it; a read path that fell back to retained content would
// serve superseded bytes.
func TestReadNeverServesRetained(t *testing.T) {
	var f file
	f.write(write(0, 16, rec(1, 0), 1))
	f.writeRetain(write(4, 8, rec(2, 0), 2))
	for off := int64(4); off < 12; off++ {
		if p, ok := f.held.lookup(off); !ok || p.Record != rec(2, 0) {
			t.Fatalf("lookup(%d) = %+v, %v, want record %+v", off, p, ok, rec(2, 0))
		}
	}
	f.held.cut(ext(4, 8))
	for off := int64(4); off < 12; off++ {
		if p, ok := f.held.lookup(off); ok {
			t.Fatalf("lookup(%d) = %+v, served a retained piece", off, p)
		}
	}
	placed, missing := f.held.span(ext(0, 16), nil, nil)
	for _, p := range placed {
		if p.Extent.Off < 12 && p.Extent.Off+p.Extent.Len > 4 {
			t.Fatalf("span served %+v inside the cut", p)
		}
	}
	if !slices.Equal(missing, []core.Extent{ext(4, 8)}) {
		t.Fatalf("span missing = %+v, want [{4 8}]", missing)
	}
	if f.retained.len() == 0 {
		t.Fatal("the retained piece was dropped by the cut")
	}
}

// TestDropRetainedBySuperseder is what a synced superseding write does: its
// retained pieces leave, and those another write keeps stay. A drop keyed on
// the extent alone would end a retention whose own write is still unsynced.
func TestDropRetainedBySuperseder(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 4, rec(2, 0), 2))
	f.writeRetain(write(4, 4, rec(3, 0), 3))
	got := f.retained.drop(ext(0, 8), bySuperseder(ver(2)), nil)
	if want := []Retained{kept(piece(0, 4, rec(1, 0), 0, 1, false), 2)}; !slices.Equal(got, want) {
		t.Fatalf("dropped %+v, want %+v", got, want)
	}
	assertRetained(t, &f, kept(piece(4, 4, rec(1, 0), 4, 1, false), 3))
}

// TestDropRetainedSplitsAtExtentEnds: an offload commit reported over the
// middle of a retained piece drops only that middle, and the tail keeps the
// right place in its record. A drop of the whole piece would lose content not
// yet offloaded; a tail with the head's skip would offer the wrong bytes.
func TestDropRetainedSplitsAtExtentEnds(t *testing.T) {
	var f file
	f.write(write(0, 12, rec(1, 32), 1))
	f.writeRetain(write(0, 12, rec(2, 0), 2))
	got := f.retained.drop(ext(4, 4), func(Retained) bool { return true }, nil)
	if want := []Retained{kept(piece(4, 4, rec(1, 32), 4, 1, false), 2)}; !slices.Equal(got, want) {
		t.Fatalf("dropped %+v, want %+v", got, want)
	}
	assertRetained(t, &f,
		kept(piece(0, 4, rec(1, 32), 0, 1, false), 2),
		kept(piece(8, 4, rec(1, 32), 8, 1, false), 2),
	)
}

// TestDropRetainedLeavesHeldUntouched: dropping retained content changes no
// held extent, even where the two overlap.
func TestDropRetainedLeavesHeldUntouched(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2))
	f.retained.drop(ext(0, 8), func(Retained) bool { return true }, nil)
	assertPieces(t, &f.held, piece(0, 8, rec(2, 0), 0, 2, false))
	assertRetained(t, &f)
}

// TestFailedWindowReholdsExactState: failing the window of one retaining write
// restores the held set exactly as it was before the write: the same entries,
// records, skips, versions and bits, and the pieces the write split joined
// back into one entry. A re-hold that re-inserted the retained piece as a
// write would lose its offloaded bit; one that did not merge would leave a
// piece of one record as several entries.
func TestFailedWindowReholdsExactState(t *testing.T) {
	var f file
	f.write(write(0, 16, rec(1, 0), 1))
	f.write(write(16, 8, rec(2, 0), 2))
	f.held.markOffloaded(ext(0, 6), ver(1), ver(1))
	before := f.held.pieces(nil)

	f.writeRetain(write(4, 16, rec(3, 0), 3))
	out := f.failWindow([]WindowRecord{failed(rec(3, 0), 3)}, nil)
	if len(out) != 0 {
		t.Fatalf("unrestored %+v, want none", out)
	}
	assertPieces(t, &f.held, before...)
	assertRetained(t, &f)
}

// TestFailWindowShuffledRestoresExactly: overlapping writes of one window,
// failed in any order, restore the state before the window. Failing the older
// first would put its retained content back where the newer write is still
// held and then lose it under the newer one's re-hold.
func TestFailWindowShuffledRestoresExactly(t *testing.T) {
	var f file
	f.write(write(0, 32, rec(1, 0), 1))
	before := f.held.pieces(nil)
	f.writeRetain(write(4, 8, rec(2, 0), 2))
	f.writeRetain(write(8, 12, rec(3, 0), 3))
	f.writeRetain(write(0, 6, rec(4, 0), 4))
	f.writeRetain(write(10, 4, rec(5, 0), 5))

	win := []WindowRecord{failed(rec(3, 0), 3), failed(rec(5, 0), 5), failed(rec(2, 0), 2), failed(rec(4, 0), 4)}
	given := slices.Clone(win)
	out := f.failWindow(win, nil)
	if len(out) != 0 {
		t.Fatalf("unrestored %+v, want none", out)
	}
	assertPieces(t, &f.held, before...)
	assertRetained(t, &f)
	if !slices.Equal(win, given) {
		t.Fatalf("failWindow reordered the caller's window: %+v", win)
	}
}

// TestFailWindowDropsItsOwnRetainedPieces: a failed record that a later,
// surviving write had superseded, and still retains, leaves no retained piece
// behind. Its record is gone, so a retained piece of it would offer bytes
// nothing vouches for.
func TestFailWindowDropsItsOwnRetainedPieces(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2))
	f.writeRetain(write(0, 8, rec(3, 0), 3)) // survives, still retaining record 2
	f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	assertPieces(t, &f.held, piece(0, 8, rec(3, 0), 0, 3, false))
	for _, r := range f.retained.list(nil) {
		if r.Record == rec(2, 0) {
			t.Fatalf("retained piece of the failed record kept: %+v", r)
		}
	}
}

// TestFailWindowReturnsUnrestorable: where a surviving newer write superseded
// the failed record, what the failed record superseded is not held again over
// the newer content. It stays retained and is returned, for the caller to keep
// under a hold or drop. Re-holding it would serve older bytes in place of a
// surviving write.
func TestFailWindowReturnsUnrestorable(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2))
	f.write(write(4, 4, rec(3, 0), 3))
	out := f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	assertPieces(t, &f.held,
		piece(0, 4, rec(1, 0), 0, 1, false),
		piece(4, 4, rec(3, 0), 0, 3, false),
	)
	want := []Retained{kept(piece(4, 4, rec(1, 0), 4, 1, false), 2)}
	if !slices.Equal(out, want) {
		t.Fatalf("unrestored %+v, want %+v", out, want)
	}
	assertRetained(t, &f, want...)
}

// TestRepointRetainedKeepsVersionAndBit: repack carries a retained record
// forward to a new place; its pieces inside the copied extent move, keeping
// their version, bit and superseder, and those outside stay. A repoint that
// dropped the superseder would orphan the retention from the write it waits on.
func TestRepointRetainedKeepsVersionAndBit(t *testing.T) {
	var f file
	f.write(write(0, 12, rec(1, 0), 1))
	f.held.markOffloaded(ext(0, 12), ver(1), ver(1))
	f.writeRetain(write(0, 12, rec(2, 0), 2))
	if n := f.retained.repoint(ext(4, 8), rec(1, 0), rec(9, 128), 0); n != 1 {
		t.Fatalf("repoint moved %d pieces, want 1", n)
	}
	assertRetained(t, &f,
		kept(piece(0, 4, rec(1, 0), 0, 1, true), 2),
		kept(piece(4, 8, rec(9, 128), 0, 1, true), 2),
	)
	if n := f.retained.repoint(ext(0, 12), rec(7, 0), rec(9, 0), 0); n != 0 {
		t.Fatalf("repoint of a record retaining nothing moved %d pieces", n)
	}
}

// TestRetainedInSegment lists only the retained pieces a segment backs, which
// repack asks while reading that segment.
func TestRetainedInSegment(t *testing.T) {
	var f file
	f.write(write(8, 4, rec(2, 0), 1))
	f.write(write(0, 4, rec(1, 0), 2))
	f.write(write(4, 4, rec(2, 64), 3))
	f.writeRetain(write(0, 12, rec(3, 0), 4))
	got := f.retained.in(2, nil)
	want := []Retained{
		kept(piece(4, 4, rec(2, 64), 0, 3, false), 4),
		kept(piece(8, 4, rec(2, 0), 0, 1, false), 4),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("in(2) = %+v, want %+v", got, want)
	}
}
