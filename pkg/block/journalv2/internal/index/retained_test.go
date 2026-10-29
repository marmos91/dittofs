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
	out, _ := f.failWindow([]WindowRecord{failed(rec(3, 0), 3)}, nil)
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
	out, _ := f.failWindow(win, nil)
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

// TestFailWindowReattributesUnderASurvivor: where a surviving newer write
// superseded the failed record, what the failed record superseded is not held
// again over the newer content: as if the failed record had never been
// written, the newer write supersedes it, and it stays retained as that
// write's. Re-holding it would serve older bytes in place of a surviving
// write.
func TestFailWindowReattributesUnderASurvivor(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2))
	f.write(write(4, 4, rec(3, 0), 3))
	out, _ := f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	assertPieces(t, &f.held,
		piece(0, 4, rec(1, 0), 0, 1, false),
		piece(4, 4, rec(3, 0), 0, 3, false),
	)
	if len(out) != 0 {
		t.Fatalf("reported %+v, want nothing", out)
	}
	assertRetained(t, &f, kept(piece(4, 4, rec(1, 0), 4, 1, false), 3))
}

// TestFailWindowReturnsNoPieceOfTheWindow: two overlapping records of one
// window, the newer since released. The newer record's pass finds the older
// record's piece it retained unrestorable, but the older record fails in the
// same call, so that piece is dropped and must not be reported as still
// retained. Reporting it would have a caller keep, under a hold, bytes of a
// record the window just declared gone.
func TestFailWindowReturnsNoPieceOfTheWindow(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(3, 0), 3))
	f.writeRetain(write(0, 4, rec(5, 0), 5))
	f.held.cut(ext(0, 4)) // released
	out, _ := f.failWindow([]WindowRecord{failed(rec(3, 0), 3), failed(rec(5, 0), 5)}, nil)
	want := []Retained{kept(piece(0, 4, rec(1, 0), 0, 1, false), 3)}
	if !slices.Equal(out, want) {
		t.Fatalf("unrestored %+v, want %+v", out, want)
	}
	assertRetained(t, &f, want...)
	assertPieces(t, &f.held, piece(4, 4, rec(1, 0), 4, 1, false))
}

// TestRemovalLeavesRetainedAndFailedWindowStopsAtIt: a truncate removes held
// content only; what a record superseded stays retained under it, for an
// offload or a hold to keep. When the record's window then fails, it is held
// again only where the record is still held, and the part the truncate took is
// reported as a gap rather than brought back past the truncation.
func TestRemovalLeavesRetainedAndFailedWindowStopsAtIt(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2))
	f.truncate(4, ver(3))
	assertRetained(t, &f, kept(piece(0, 8, rec(1, 0), 0, 1, false), 2))
	out, covers := f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	want := []Retained{kept(piece(4, 4, rec(1, 0), 4, 1, false), 2)}
	if !slices.Equal(out, want) || len(covers) != 0 {
		t.Fatalf("unrestored %+v covers %v, want %+v and none", out, covers, want)
	}
	assertRetained(t, &f, want...)
	assertPieces(t, &f.held, piece(0, 4, rec(1, 0), 0, 1, false))
}

// TestFailedWindowThenSurvivorFails: a write surviving one failed window, in
// the next segment, is itself failed later. What the first window could not
// restore under it must be held again then, so the last synced write is
// served. Leaving that part retained by the first failed record, whose version
// never comes round again, turns it into a gap.
func TestFailedWindowThenSurvivorFails(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))       // synced
	f.writeRetain(write(0, 8, rec(2, 0), 2)) // the window that fails
	f.writeRetain(write(0, 4, rec(3, 0), 3)) // the next segment
	f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	f.failWindow([]WindowRecord{failed(rec(3, 0), 3)}, nil)
	assertPieces(t, &f.held, piece(0, 8, rec(1, 0), 0, 1, false))
	assertRetained(t, &f)
}

// TestFailedWindowThenSurvivorSyncs: the same, but the surviving write syncs.
// What it superseded must then leave by its version like anything else it
// retains, or it stays retained for good and pins its segment.
func TestFailedWindowThenSurvivorSyncs(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))
	f.writeRetain(write(0, 8, rec(2, 0), 2))
	f.writeRetain(write(0, 4, rec(3, 0), 3))
	out, _ := f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	if len(out) != 0 {
		t.Fatalf("reported %+v, want nothing: a surviving write covers the part", out)
	}
	assertRetained(t, &f, kept(piece(0, 4, rec(1, 0), 0, 1, false), 3))
	f.retained.drop(whole, bySuperseder(ver(3)), nil)
	assertRetained(t, &f)
}

// TestFailedWindowNamesTheCoveringVersions: the surviving write syncs before
// the earlier window's failure is handled, so its sync has already dropped
// what it retained when the failed window hands it a part. failWindow names
// the versions it handed parts to, so the caller, which knows that one has
// synced, drops the part at once. Without the name the part stays retained
// for good, pinning its segment.
func TestFailedWindowNamesTheCoveringVersions(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))                // synced
	f.writeRetain(write(0, 8, rec(2, 0), 2))          // the window that fails
	f.writeRetain(write(0, 4, rec(3, 0), 3))          // the next segment
	f.retained.drop(whole, bySuperseder(ver(3)), nil) // which syncs first
	out, covers := f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	if len(out) != 0 {
		t.Fatalf("reported %+v, want nothing", out)
	}
	if want := []core.Version{ver(3)}; !slices.Equal(covers, want) {
		t.Fatalf("covering versions %+v, want %+v", covers, want)
	}
	for _, v := range covers {
		f.retained.drop(whole, bySuperseder(v), nil)
	}
	assertRetained(t, &f)
}

// TestFailedWindowReportsOnlyAGap: where a failed record's superseded content
// lies under neither the record nor a newer write, a removal or release took
// it since, so it is not held again. It is reported, still retained, for the
// caller to keep under a hold or drop. Content held there that is older than
// the part, as a fill can be, does not supersede it and counts as a gap.
func TestFailedWindowReportsOnlyAGap(t *testing.T) {
	var f file
	f.write(write(0, 12, rec(1, 0), 5))
	f.writeRetain(write(0, 12, rec(2, 0), 6))
	f.write(write(0, 4, rec(3, 0), 7))
	f.held.cut(ext(0, 12)) // released, say
	f.held.fill(Placed{Extent: ext(8, 4), Record: rec(4, 0), Version: ver(1)})
	f.write(write(0, 4, rec(5, 0), 8))
	out, _ := f.failWindow([]WindowRecord{failed(rec(2, 0), 6)}, nil)
	want := []Retained{kept(piece(4, 8, rec(1, 0), 4, 5, false), 6)}
	if !slices.Equal(out, want) {
		t.Fatalf("reported %+v, want %+v", out, want)
	}
	assertRetained(t, &f, kept(piece(0, 4, rec(1, 0), 0, 5, false), 8), want[0])
}

// TestFailedWindowReholdsNewestOfOverlapping: once an earlier failed window
// gives a retained piece a surviving record as superseder, that record can
// retain two pieces at one offset. When its own window fails, the newer is
// held again and the older stays retained as superseded by it. Holding them
// back in offset order would serve the older; consuming both would lose the
// older, which the newer's own failure would need.
func TestFailedWindowReholdsNewestOfOverlapping(t *testing.T) {
	var f file
	f.write(write(0, 8, rec(1, 0), 1))       // Q, synced
	f.writeRetain(write(0, 8, rec(2, 0), 2)) // B, window 1
	f.write(write(4, 4, rec(3, 0), 3))       // Y, over B
	f.writeRetain(write(0, 8, rec(4, 0), 4)) // X, window 2
	f.failWindow([]WindowRecord{failed(rec(2, 0), 2)}, nil)
	f.failWindow([]WindowRecord{failed(rec(4, 0), 4)}, nil)
	assertPieces(t, &f.held,
		piece(0, 4, rec(1, 0), 0, 1, false),
		piece(4, 4, rec(3, 0), 0, 3, false),
	)
	assertRetained(t, &f, kept(piece(4, 4, rec(1, 0), 4, 1, false), 3))
}

// TestRepointRetainedKeepsVersionAndBit: repack carries a retained record
// forward to a new place; its pieces inside the copied extent move, keeping
// their version, bit and superseder, and those outside stay. A repoint that
// dropped the superseder would orphan the retention from the write it waits on.
// The copy starts before the piece, so the piece's bytes sit in it past skip
// by their distance from the copy's start; a repoint that set skip alone would
// offer the copy's first bytes for them.
func TestRepointRetainedKeepsVersionAndBit(t *testing.T) {
	var f file
	f.write(write(4, 8, rec(1, 0), 1))
	f.held.markOffloaded(ext(4, 8), ver(1), ver(1))
	f.writeRetain(write(0, 12, rec(2, 0), 2))
	if n := f.retained.repoint(ext(0, 10), rec(1, 0), rec(9, 128), 100); n != 1 {
		t.Fatalf("repoint moved %d pieces, want 1", n)
	}
	assertRetained(t, &f,
		kept(piece(4, 6, rec(9, 128), 104, 1, true), 2),
		kept(piece(10, 2, rec(1, 0), 6, 1, true), 2),
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

// TestRetainedMarkOffloaded: the retained offloaded-bit setter. A mark inside
// e sets the bit of only the pieces whose version lies in the window and
// whose bit is unset, splitting a piece at e's ends and keeping each part's
// superseder, record and skip right. A mark that set a bit outside the window
// would mark content a ref does not cover, which eviction would then drop;
// one that re-set an already-set bit would miscount to the caller.
func TestRetainedMarkOffloaded(t *testing.T) {
	// The window excludes pieces outside it, sets only unset bits, and
	// counts what it changed. Two retained pieces of different versions: the
	// v1 piece came retained already bit-set — the held mark set the content
	// before the write superseded it — and the v2 piece is unset. A window
	// covering both changes the unset one only; a second one changes
	// nothing, and a window between the pieces' versions, holding neither,
	// changes nothing.
	t.Run("window and count", func(t *testing.T) {
		var f file
		f.write(write(0, 8, rec(1, 0), 1))
		f.held.markOffloaded(ext(0, 8), ver(1), ver(1))
		f.writeRetain(write(0, 8, rec(2, 0), 2)) // retains v1, bit-set
		f.writeRetain(write(0, 8, rec(3, 0), 3)) // retains v2, bit-unset
		// Retained now: the v1 piece, bit-set, and the v2 piece, bit-unset.
		assertRetained(t, &f,
			kept(piece(0, 8, rec(1, 0), 0, 1, true), 2),
			kept(piece(0, 8, rec(2, 0), 0, 2, false), 3),
		)
		if n := f.retained.markOffloaded(ext(0, 8), ver(1), ver(9)); n != 1 {
			t.Fatalf("markOffloaded changed %d pieces, want the unset v2 one only", n)
		}
		assertRetained(t, &f,
			kept(piece(0, 8, rec(1, 0), 0, 1, true), 2),
			kept(piece(0, 8, rec(2, 0), 0, 2, true), 3),
		)
		if n := f.retained.markOffloaded(ext(0, 8), ver(1), ver(9)); n != 0 {
			t.Fatalf("a second markOffloaded changed %d pieces, want 0: a set bit stays", n)
		}
		if n := f.retained.markOffloaded(ext(0, 8), ver(4), ver(5)); n != 0 {
			t.Fatalf("markOffloaded in a window between the pieces' versions changed %d pieces, want 0", n)
		}
	})

	// A piece straddling both ends of e is split in three: the middle part
	// takes the bit, the head and tail keep their unset bits, and each part
	// keeps its record and skip right. A mark over the middle that kept the
	// head's skip for the middle would offer the wrong bytes when the
	// offload reads the part back.
	t.Run("split at the ends", func(t *testing.T) {
		var f file
		f.write(write(0, 12, rec(1, 64), 1))
		f.writeRetain(write(0, 12, rec(2, 0), 3))
		if n := f.retained.markOffloaded(ext(4, 4), ver(1), ver(3)); n != 1 {
			t.Fatalf("markOffloaded changed %d pieces, want the split one", n)
		}
		assertRetained(t, &f,
			kept(piece(0, 4, rec(1, 64), 0, 1, false), 3),
			kept(piece(4, 4, rec(1, 64), 4, 1, true), 3),
			kept(piece(8, 4, rec(1, 64), 8, 1, false), 3),
		)
		assertPieces(t, &f.held, piece(0, 12, rec(2, 0), 0, 3, false))
	})

	// Two overlapping retained pieces are both marked inside e: a mark that
	// stopped at the first would leave the older version's bytes unmarked.
	t.Run("overlapping pieces", func(t *testing.T) {
		var f file
		f.write(write(0, 8, rec(1, 0), 1))
		f.writeRetain(write(0, 8, rec(2, 0), 2)) // retains v1 [0,8)
		f.writeRetain(write(4, 8, rec(3, 0), 3)) // retains v2's [4,8) part
		if n := f.retained.markOffloaded(ext(0, 12), ver(1), ver(3)); n != 2 {
			t.Fatalf("markOffloaded changed %d pieces, want both overlapping ones", n)
		}
		assertRetained(t, &f,
			kept(piece(0, 8, rec(1, 0), 0, 1, true), 2),
			kept(piece(4, 4, rec(2, 0), 4, 2, true), 3),
		)
	})

	// A mark of a gap changes nothing and reports zero.
	t.Run("nothing inside", func(t *testing.T) {
		var f file
		f.writeRetain(write(0, 4, rec(1, 0), 1))
		if n := f.retained.markOffloaded(ext(100, 4), ver(0), ver(9)); n != 0 {
			t.Fatalf("markOffloaded over a gap changed %d pieces", n)
		}
		if n := f.retained.markOffloaded(ext(0, 0), ver(0), ver(9)); n != 0 {
			t.Fatalf("markOffloaded of an empty extent changed %d pieces", n)
		}
	})
}

// TestRetainedUnmark: the retained unmark clears the bit of the parts inside
// e of every piece at most newest with its bit set, keeping each part's
// superseder, record and skip, and counts what it cleared. An unmark that
// cleared a bit above its version would hide a commit the journal was told
// of; one that cleared an already-clear bit would miscount to the caller.
func TestRetainedUnmark(t *testing.T) {
	t.Run("clears only set bits in the window", func(t *testing.T) {
		var f file
		f.write(write(0, 8, rec(1, 0), 2))
		f.writeRetain(write(0, 8, rec(2, 0), 3))
		f.retained.markOffloaded(ext(0, 8), ver(0), ver(2))
		if n := f.retained.unmark(ext(0, 8), ver(1)); n != 0 {
			t.Fatalf("unmark below the piece's version cleared %d parts, want 0", n)
		}
		if n := f.retained.unmark(ext(0, 8), ver(2)); n != 1 {
			t.Fatalf("unmark cleared %d parts, want 1", n)
		}
		assertRetained(t, &f, kept(piece(0, 8, rec(1, 0), 0, 2, false), 3))
	})

	t.Run("splits and keeps", func(t *testing.T) {
		var f file
		f.write(write(0, 16, rec(1, 0), 1))
		f.held.markOffloaded(whole, ver(1), ver(1))
		f.writeRetain(write(0, 16, rec(2, 0), 2))
		if n := f.retained.unmark(ext(4, 4), ver(1)); n != 1 {
			t.Fatalf("unmark cleared %d parts, want the middle", n)
		}
		assertRetained(t, &f,
			kept(piece(0, 4, rec(1, 0), 0, 1, true), 2),
			kept(piece(4, 4, rec(1, 0), 4, 1, false), 2),
			kept(piece(8, 8, rec(1, 0), 8, 1, true), 2),
		)
	})
}
