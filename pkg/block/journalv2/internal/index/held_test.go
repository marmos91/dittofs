package index

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

func ver(n uint64) core.Version { return core.Version{Counter: n} }

func ext(off, n int64) core.Extent { return core.Extent{Off: off, Len: n} }

func rec(seg core.SegmentID, off uint32) RecordAt { return RecordAt{Segment: seg, Offset: off} }

// write is a client write: a fresh record holding n bytes at off, dirty.
func write(off, n int64, r RecordAt, v uint64) Placed {
	return Placed{Extent: ext(off, n), Record: r, Version: ver(v)}
}

// assertPieces checks the set's read-out against want, and the structural
// invariants every state must keep.
func assertPieces(t *testing.T, s *heldSet, want ...Piece) {
	t.Helper()
	got := s.pieces(nil)
	if !slices.Equal(got, want) {
		t.Fatalf("pieces:\n got  %+v\n want %+v", got, want)
	}
	if s.len() != len(want) {
		t.Fatalf("len() = %d, want %d", s.len(), len(want))
	}
	checkCanonical(t, got)
}

// checkCanonical asserts that pieces are disjoint, in offset order, and
// no two adjacent pieces are one contiguous piece of one record that should
// have been a single entry.
func checkCanonical(t *testing.T, ps []Piece) {
	t.Helper()
	for i, p := range ps {
		if p.Extent.Len <= 0 {
			t.Fatalf("piece %d is empty: %+v", i, p)
		}
		if i == 0 {
			continue
		}
		q := ps[i-1]
		if q.Extent.Off+q.Extent.Len > p.Extent.Off {
			t.Fatalf("pieces %d and %d overlap: %+v, %+v", i-1, i, q, p)
		}
		if mergeable(q, p) {
			t.Fatalf("pieces %d and %d are one piece of one record kept as two entries: %+v, %+v", i-1, i, q, p)
		}
	}
}

func mergeable(q, p Piece) bool {
	return q.Extent.Off+q.Extent.Len == p.Extent.Off && q.Record == p.Record &&
		q.Version == p.Version && q.Offloaded == p.Offloaded &&
		int64(q.Skip)+q.Extent.Len == int64(p.Skip)
}

// TestInsertSplitsOneExtentIntoThree is the three-way split: a write inside
// one held extent leaves its head and tail held from the old record, each at
// the right place in it. A split that forgets to advance the tail's skip would
// serve the head's bytes for the tail.
func TestInsertSplitsOneExtentIntoThree(t *testing.T) {
	var s heldSet
	a, b := rec(1, 0), rec(1, 4096)
	s.insert(write(0, 12, a, 7))
	s.insert(write(4, 4, b, 8))
	assertPieces(t, &s,
		Piece{Extent: ext(0, 4), Record: a, Skip: 0, Version: ver(7)},
		Piece{Extent: ext(4, 4), Record: b, Skip: 0, Version: ver(8)},
		Piece{Extent: ext(8, 4), Record: a, Skip: 8, Version: ver(7)},
	)
}

// TestInsertCoversSeveralExtentsAndPartsOfTwo is the other overwrite shape:
// the write drops every extent it covers whole and narrows the two it covers
// in part, so no offset is left under two extents.
func TestInsertCoversSeveralExtentsAndPartsOfTwo(t *testing.T) {
	var s heldSet
	for i := range int64(5) {
		s.insert(write(10*i, 10, rec(1, uint32(i)*100), uint64(i+1)))
	}
	n := rec(2, 0)
	s.insert(write(5, 37, n, 9)) // [5, 42): parts of 0 and 4, all of 1, 2, 3
	assertPieces(t, &s,
		Piece{Extent: ext(0, 5), Record: rec(1, 0), Version: ver(1)},
		Piece{Extent: ext(5, 37), Record: n, Version: ver(9)},
		Piece{Extent: ext(42, 8), Record: rec(1, 400), Skip: 2, Version: ver(5)},
	)
}

// TestInsertNarrowsEitherEnd covers a write over exactly one end of an extent:
// the head kept at its own skip, and the tail kept at an advanced one.
func TestInsertNarrowsEitherEnd(t *testing.T) {
	var s heldSet
	a := rec(1, 0)
	s.insert(write(0, 8, a, 1))
	s.insert(write(0, 3, rec(1, 100), 2))
	s.insert(write(6, 2, rec(1, 200), 3))
	assertPieces(t, &s,
		Piece{Extent: ext(0, 3), Record: rec(1, 100), Version: ver(2)},
		Piece{Extent: ext(3, 3), Record: a, Skip: 3, Version: ver(1)},
		Piece{Extent: ext(6, 2), Record: rec(1, 200), Version: ver(3)},
	)
}

// TestOverwriteSameExtentManyTimes is the churn case: rewriting one extent
// over and over keeps one entry, whatever the version and record.
func TestOverwriteSameExtentManyTimes(t *testing.T) {
	var s heldSet
	for i := range uint64(1000) {
		s.insert(write(4096, 4096, rec(core.SegmentID(1+i/100), uint32(i)), i+1))
	}
	assertPieces(t, &s, Piece{Extent: ext(4096, 4096), Record: rec(10, 999), Version: ver(1000)})
}

// TestZeroLengthInsertHoldsNothing: a zero-length write adds no entry, and
// splits nothing it lands inside.
func TestZeroLengthInsertHoldsNothing(t *testing.T) {
	var s heldSet
	s.insert(write(0, 8, rec(1, 0), 1))
	s.insert(write(4, 0, rec(1, 100), 2))
	assertPieces(t, &s, Piece{Extent: ext(0, 8), Record: rec(1, 0), Version: ver(1)})
}

// TestHeldContentFarFromZero: content need not start at offset zero nor
// be contiguous, and the gaps before, between and after it are reported
// exactly, never widened.
func TestHeldContentFarFromZero(t *testing.T) {
	var s heldSet
	const gib = int64(1) << 30
	s.insert(write(gib, 64<<10, rec(1, 0), 1))
	s.insert(write(3*gib, 4096, rec(1, 1<<20), 2))

	held, missing := s.span(ext(0, 4*gib), nil, nil)
	wantHeld := []Placed{
		{Extent: ext(gib, 64<<10), Record: rec(1, 0), Version: ver(1)},
		{Extent: ext(3*gib, 4096), Record: rec(1, 1<<20), Version: ver(2)},
	}
	wantMissing := []core.Extent{ext(0, gib), ext(gib+64<<10, 2*gib-64<<10), ext(3*gib+4096, gib-4096)}
	if !slices.Equal(held, wantHeld) || !slices.Equal(missing, wantMissing) {
		t.Fatalf("span:\n held    %+v\n missing %+v\nwant\n held    %+v\n missing %+v", held, missing, wantHeld, wantMissing)
	}
	if _, ok := s.lookup(gib - 1); ok {
		t.Error("lookup(1 GiB - 1) found an extent below the first held byte")
	}
	if p, ok := s.lookup(gib + 100); !ok || p.Extent != ext(gib, 64<<10) {
		t.Errorf("lookup(1 GiB + 100) = %+v, %v; want the extent at 1 GiB", p, ok)
	}
}

// TestSpanClipsToTheQuery: held pieces are cut to the span asked for, with
// the skip advanced to the first byte served, and gaps never reach outside it.
func TestSpanClipsToTheQuery(t *testing.T) {
	var s heldSet
	s.insert(write(0, 10, rec(1, 0), 1))
	s.insert(write(20, 10, rec(1, 100), 2))
	held, missing := s.span(ext(5, 20), nil, nil)
	wantHeld := []Placed{
		{Extent: ext(5, 5), Record: rec(1, 0), Skip: 5, Version: ver(1)},
		{Extent: ext(20, 5), Record: rec(1, 100), Version: ver(2)},
	}
	if !slices.Equal(held, wantHeld) || !slices.Equal(missing, []core.Extent{ext(10, 10)}) {
		t.Fatalf("span = %+v, %+v", held, missing)
	}
	if held, missing := s.span(ext(5, 0), nil, nil); len(held)+len(missing) != 0 {
		t.Errorf("an empty span returned %+v, %+v", held, missing)
	}
	if held, missing := s.span(ext(12, 4), nil, nil); len(held) != 0 || !slices.Equal(missing, []core.Extent{ext(12, 4)}) {
		t.Errorf("a span inside a gap returned %+v, %+v", held, missing)
	}
}

// TestWriteNearLargestOffset is a write close to the largest file
// offset: keys near the top of the range neither wrap nor lose their tail.
func TestWriteNearLargestOffset(t *testing.T) {
	var s heldSet
	off := int64(math.MaxInt64 - 8192)
	s.insert(write(off, 8192, rec(1, 0), 1))
	s.insert(write(off+4096, 1, rec(1, 100), 2))
	assertPieces(t, &s,
		Piece{Extent: ext(off, 4096), Record: rec(1, 0), Version: ver(1)},
		Piece{Extent: ext(off+4096, 1), Record: rec(1, 100), Version: ver(2)},
		Piece{Extent: ext(off+4097, 4095), Record: rec(1, 0), Skip: 4097, Version: ver(1)},
	)
	if p, ok := s.lookup(math.MaxInt64 - 1); !ok || p.Record != rec(1, 0) {
		t.Errorf("lookup at the top = %+v, %v", p, ok)
	}
}

// TestReadIgnoresTheOffloadedBit: what a read is told — held pieces,
// gaps, records and versions — is the same however much of the file is marked
// offloaded. Marking half of a record splits its entry; a read that showed
// that split would be answering from the bit.
func TestReadIgnoresTheOffloadedBit(t *testing.T) {
	var s heldSet
	s.insert(write(0, 16, rec(1, 0), 1))
	s.insert(write(20, 8, rec(1, 100), 2))
	beforeHeld, beforeMissing := s.span(ext(0, 32), nil, nil)
	beforeAt, _ := s.lookup(10)

	if n := s.markOffloaded(ext(4, 4), ver(1), ver(2)); n != 1 {
		t.Fatalf("markOffloaded marked %d pieces, want 1", n)
	}
	if s.len() != 4 {
		t.Fatalf("a mark inside one entry left %d entries, want 4", s.len())
	}
	afterHeld, afterMissing := s.span(ext(0, 32), nil, nil)
	if !slices.Equal(afterHeld, beforeHeld) || !slices.Equal(afterMissing, beforeMissing) {
		t.Fatalf("span changed with the offloaded bit:\n before %+v %+v\n after  %+v %+v", beforeHeld, beforeMissing, afterHeld, afterMissing)
	}
	afterAt, _ := s.lookup(10)
	if afterAt.Record != beforeAt.Record || afterAt.Version != beforeAt.Version ||
		int64(afterAt.Skip)+10-afterAt.Extent.Off != int64(beforeAt.Skip)+10-beforeAt.Extent.Off {
		t.Fatalf("lookup(10) places the byte differently once marked: %+v, then %+v", beforeAt, afterAt)
	}
}

// TestFillInsertsOnlyAbsent is a fill over content partly held,
// partly dirty and partly absent: the held parts keep their record, version
// and bit, and only the gaps take the filled record, marked offloaded.
func TestFillInsertsOnlyAbsent(t *testing.T) {
	var s heldSet
	s.insert(write(4, 4, rec(1, 0), 5)) // dirty
	s.insert(write(12, 4, rec(1, 100), 3))
	s.markOffloaded(ext(12, 4), ver(3), ver(3)) // held and offloaded
	f := rec(2, 0)
	s.fill(Placed{Extent: ext(0, 20), Record: f, Version: ver(3)})
	assertPieces(t, &s,
		Piece{Extent: ext(0, 4), Record: f, Version: ver(3), Offloaded: true},
		Piece{Extent: ext(4, 4), Record: rec(1, 0), Version: ver(5)},
		Piece{Extent: ext(8, 4), Record: f, Skip: 8, Version: ver(3), Offloaded: true},
		Piece{Extent: ext(12, 4), Record: rec(1, 100), Version: ver(3), Offloaded: true},
		Piece{Extent: ext(16, 4), Record: f, Skip: 16, Version: ver(3), Offloaded: true},
	)
}

// TestReleaseMiddleOfOneRecord is a release of the middle of one record:
// both ends stay held from it, and nothing is held in between. The released
// piece comes back with its version, which the release record names.
func TestReleaseMiddleOfOneRecord(t *testing.T) {
	var s heldSet
	a := rec(1, 0)
	s.insert(write(0, 12, a, 7))
	s.markOffloaded(ext(0, 12), ver(7), ver(7))
	out, err := s.release(ext(4, 4), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []core.Held{{Extent: ext(4, 4), Version: ver(7)}}; !slices.Equal(out, want) {
		t.Fatalf("release returned %+v, want %+v", out, want)
	}
	assertPieces(t, &s,
		Piece{Extent: ext(0, 4), Record: a, Version: ver(7), Offloaded: true},
		Piece{Extent: ext(8, 4), Record: a, Skip: 8, Version: ver(7), Offloaded: true},
	)
}

// TestReleaseOneEnd: releasing the tail leaves the head at its own skip, and
// releasing the head leaves the tail at an advanced one.
func TestReleaseOneEnd(t *testing.T) {
	var s heldSet
	a := rec(1, 0)
	s.insert(write(0, 12, a, 7))
	s.markOffloaded(ext(0, 12), ver(7), ver(7))
	if _, err := s.release(ext(8, 4), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.release(ext(0, 2), nil); err != nil {
		t.Fatal(err)
	}
	assertPieces(t, &s, Piece{Extent: ext(2, 6), Record: a, Skip: 2, Version: ver(7), Offloaded: true})
}

// TestReleaseEveryPartSeparately is a release of every part of one
// record in separate calls: the file ends up holding nothing, and the calls
// together return exactly the record's extent.
func TestReleaseEveryPartSeparately(t *testing.T) {
	var s heldSet
	s.insert(write(0, 12, rec(1, 0), 7))
	s.markOffloaded(ext(0, 12), ver(7), ver(7))
	var out []core.Held
	for _, e := range []core.Extent{ext(4, 4), ext(0, 4), ext(8, 4)} {
		var err error
		if out, err = s.release(e, out); err != nil {
			t.Fatal(err)
		}
	}
	assertPieces(t, &s)
	var total int64
	for _, h := range out {
		total += h.Extent.Len
	}
	if total != 12 {
		t.Fatalf("released %d bytes in all, want 12: %+v", total, out)
	}
}

// TestReleasePartlyOffloadedIsRefusedWhole is a release of an
// extent only part of which is offloaded: the call is refused and nothing,
// not even the offloaded part, stops being held.
func TestReleasePartlyOffloadedIsRefusedWhole(t *testing.T) {
	var s heldSet
	s.insert(write(0, 8, rec(1, 0), 1))
	s.insert(write(8, 8, rec(1, 100), 2))
	s.markOffloaded(ext(0, 12), ver(1), ver(2))
	before := s.pieces(nil)
	if s.allOffloaded(ext(0, 16)) {
		t.Fatal("allOffloaded over a partly dirty span is true")
	}
	if !s.allOffloaded(ext(0, 12)) {
		t.Fatal("allOffloaded over a fully marked span is false")
	}
	out, err := s.release(ext(0, 16), nil)
	if !errors.Is(err, ErrNotOffloaded) {
		t.Fatalf("release of a partly dirty span returned %v, want ErrNotOffloaded", err)
	}
	if len(out) != 0 {
		t.Fatalf("a refused release returned pieces %+v", out)
	}
	assertPieces(t, &s, before...)
}

// TestReleaseOverAGapIsAllowed: content not held carries no bit to refuse, so
// a release over a gap and some offloaded content releases the content.
func TestReleaseOverAGapIsAllowed(t *testing.T) {
	var s heldSet
	s.insert(write(10, 10, rec(1, 0), 1))
	s.markOffloaded(ext(10, 10), ver(1), ver(1))
	out, err := s.release(ext(0, 30), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []core.Held{{Extent: ext(10, 10), Version: ver(1)}}; !slices.Equal(out, want) {
		t.Fatalf("release returned %+v", out)
	}
	assertPieces(t, &s)
}

// TestMarkLeavesANewerVersionDirty is the offer the RFC walks through: it
// covered [0, 4) at v10, a client wrote [1, 2) at v11 meanwhile, and the
// report names [0, 4).
// Only the v10 pieces are marked: marking v11 would let eviction drop the only
// copy of it.
func TestMarkLeavesANewerVersionDirty(t *testing.T) {
	var s heldSet
	a, b := rec(1, 0), rec(1, 100)
	s.insert(write(0, 4, a, 10))
	s.insert(write(1, 1, b, 11))
	if n := s.markOffloaded(ext(0, 4), ver(10), ver(10)); n != 2 {
		t.Fatalf("marked %d pieces, want 2", n)
	}
	assertPieces(t, &s,
		Piece{Extent: ext(0, 1), Record: a, Version: ver(10), Offloaded: true},
		Piece{Extent: ext(1, 1), Record: b, Version: ver(11)},
		Piece{Extent: ext(2, 2), Record: a, Skip: 2, Version: ver(10), Offloaded: true},
	)
}

// TestMarkMergesBackToOneEntry: marking one record piece in two halves
// splits it and then rejoins it, so a fully marked piece is one entry again,
// not two that cost the index twice.
func TestMarkMergesBackToOneEntry(t *testing.T) {
	for _, at := range []int64{1, 4, 7} {
		var s heldSet
		a := rec(1, 0)
		s.insert(write(0, 8, a, 1))
		s.markOffloaded(ext(at, 8-at), ver(1), ver(1))
		s.markOffloaded(ext(0, at), ver(1), ver(1))
		assertPieces(t, &s, Piece{Extent: ext(0, 8), Record: a, Version: ver(1), Offloaded: true})
	}
}

// TestMarkIsOnlyWithinTheSpan: a report marks exactly what it names, never a
// neighbouring piece of the same record.
func TestMarkIsOnlyWithinTheSpan(t *testing.T) {
	var s heldSet
	a := rec(1, 0)
	s.insert(write(0, 12, a, 1))
	s.markOffloaded(ext(4, 4), ver(1), ver(1))
	assertPieces(t, &s,
		Piece{Extent: ext(0, 4), Record: a, Version: ver(1)},
		Piece{Extent: ext(4, 4), Record: a, Skip: 4, Version: ver(1), Offloaded: true},
		Piece{Extent: ext(8, 4), Record: a, Skip: 8, Version: ver(1)},
	)
}

// TestReappendMovesItsEntry is the regression that a record re-appended after
// a failed sync keeps holding exactly what it held, from its new place: every
// piece of it, split by a later write, moves with its skip, version and bit,
// and no other record's piece moves.
func TestReappendMovesItsEntry(t *testing.T) {
	var s heldSet
	a, b, moved := rec(3, 0), rec(3, 4096), rec(4, 512)
	s.insert(write(0, 12, a, 1))
	s.insert(write(4, 4, b, 2))
	s.markOffloaded(ext(8, 4), ver(1), ver(1))
	if n := s.repoint(ext(0, 12), a, moved, 0); n != 2 {
		t.Fatalf("repoint moved %d pieces, want 2", n)
	}
	assertPieces(t, &s,
		Piece{Extent: ext(0, 4), Record: moved, Version: ver(1)},
		Piece{Extent: ext(4, 4), Record: b, Version: ver(2)},
		Piece{Extent: ext(8, 4), Record: moved, Skip: 8, Version: ver(1), Offloaded: true},
	)
}

// TestCarriedHeaderOnlyRecordLeavesHeldUntouched is the regression that
// carrying a header-only record forward changes no held extent. Here a release
// at v7 was followed by a fill of the same bytes at v7; repack carries the
// release record to a new place. The fill must stay held and unmoved: a
// repoint that moved pieces of records other than the one it names would
// move the fill to the carried release record's place.
func TestCarriedHeaderOnlyRecordLeavesHeldUntouched(t *testing.T) {
	var s heldSet
	w, release, fill := rec(12, 0), rec(13, 0), rec(14, 0)
	s.insert(write(0, 8, w, 7))
	s.markOffloaded(ext(0, 8), ver(7), ver(7))
	if _, err := s.release(ext(0, 4), nil); err != nil {
		t.Fatal(err)
	}
	s.fill(Placed{Extent: ext(0, 4), Record: fill, Version: ver(7)})
	before := s.pieces(nil)
	if n := s.repoint(ext(0, 4), release, rec(15, 0), 0); n != 0 {
		t.Fatalf("repoint of a header-only record moved %d pieces", n)
	}
	assertPieces(t, &s, before...)
}

// TestOneRecordJoinsOnlyWhereContiguous: two placements of one record are one
// piece only where both the file offsets and the payload offsets run on, and
// only at one version. A
// merge that checked only the record would serve the wrong payload bytes for
// the second placement, or a read would be told a gap is held.
func TestOneRecordJoinsOnlyWhereContiguous(t *testing.T) {
	r := rec(1, 0)
	var payloadJump heldSet
	payloadJump.insert(Placed{Extent: ext(0, 4), Record: r, Version: ver(1)})
	payloadJump.insert(Placed{Extent: ext(4, 4), Record: r, Skip: 100, Version: ver(1)})
	assertPieces(t, &payloadJump,
		Piece{Extent: ext(0, 4), Record: r, Version: ver(1)},
		Piece{Extent: ext(4, 4), Record: r, Skip: 100, Version: ver(1)},
	)
	if held, _ := payloadJump.span(ext(0, 8), nil, nil); len(held) != 2 {
		t.Fatalf("span joined a payload jump: %+v", held)
	}

	var twoVersions heldSet
	twoVersions.insert(Placed{Extent: ext(0, 4), Record: r, Version: ver(1)})
	twoVersions.insert(Placed{Extent: ext(4, 4), Record: r, Skip: 4, Version: ver(2)})
	if twoVersions.len() != 2 {
		t.Fatalf("one record named with two versions became %d entries, want 2", twoVersions.len())
	}
	if held, _ := twoVersions.span(ext(0, 8), nil, nil); len(held) != 2 {
		t.Fatalf("span joined two versions: %+v", held)
	}

	var fileGap heldSet
	fileGap.insert(Placed{Extent: ext(0, 4), Record: r, Version: ver(1)})
	fileGap.insert(Placed{Extent: ext(8, 4), Record: r, Skip: 4, Version: ver(1)})
	held, missing := fileGap.span(ext(0, 12), nil, nil)
	if len(held) != 2 || !slices.Equal(missing, []core.Extent{ext(4, 4)}) {
		t.Fatalf("span joined across a gap in the file: %+v, %+v", held, missing)
	}
}

// TestSpanKeepsTheCallersEntries: a read appends to a buffer that may already
// hold another read's pieces, and must not widen one of those even when the
// new piece continues it. Two spans of one record read back to back stay two
// entries.
func TestSpanKeepsTheCallersEntries(t *testing.T) {
	var s heldSet
	s.insert(write(0, 8, rec(1, 0), 1))
	held, _ := s.span(ext(0, 4), nil, nil)
	held, _ = s.span(ext(4, 4), held, nil)
	want := []Placed{
		{Extent: ext(0, 4), Record: rec(1, 0), Version: ver(1)},
		{Extent: ext(4, 4), Record: rec(1, 0), Skip: 4, Version: ver(1)},
	}
	if !slices.Equal(held, want) {
		t.Fatalf("span = %+v, want %+v", held, want)
	}
}

// TestRecordFillingASegment: a record as long as a segment, whose last byte
// sits at the segment's last payload offset, splits and rejoins without the
// 32-bit skip and length wrapping.
func TestRecordFillingASegment(t *testing.T) {
	var s heldSet
	a := rec(1, 0)
	s.insert(write(0, core.SegmentCap, a, 1))
	s.markOffloaded(ext(core.SegmentCap-1, 1), ver(1), ver(1))
	assertPieces(t, &s,
		Piece{Extent: ext(0, core.SegmentCap-1), Record: a, Version: ver(1)},
		Piece{Extent: ext(core.SegmentCap-1, 1), Record: a, Skip: core.SegmentCap - 1, Version: ver(1), Offloaded: true},
	)
	s.markOffloaded(ext(0, core.SegmentCap), ver(1), ver(1))
	assertPieces(t, &s, Piece{Extent: ext(0, core.SegmentCap), Record: a, Version: ver(1), Offloaded: true})
}

// TestRepackCopyCoalesces is a repack copy: adjacent held extents of one file
// that share a version and a bit, from two records, are copied into one new
// record at consecutive payload offsets, and become one entry. A repoint that
// kept the old skips would point the second extent at the wrong bytes of the
// copy, and the two would never merge.
func TestRepackCopyCoalesces(t *testing.T) {
	var s heldSet
	a, b, c, copied := rec(1, 0), rec(1, 4096), rec(1, 8192), rec(9, 0)
	s.insert(write(0, 8, a, 3))
	s.insert(write(8, 8, b, 3))
	s.insert(write(4, 2, c, 5)) // splits a; stays where it is
	s.markOffloaded(ext(0, 16), ver(3), ver(3))
	// The copy holds a's [6, 8) at 0 and b's [8, 16) at 2.
	s.repoint(ext(6, 2), a, copied, 0)
	s.repoint(ext(8, 8), b, copied, 2)
	assertPieces(t, &s,
		Piece{Extent: ext(0, 4), Record: a, Version: ver(3), Offloaded: true},
		Piece{Extent: ext(4, 2), Record: c, Version: ver(5)},
		Piece{Extent: ext(6, 10), Record: copied, Version: ver(3), Offloaded: true},
	)
}

// TestUnmark: the held offloaded-bit clearer, the inverse of markOffloaded.
// It clears the bit of the parts inside e of every piece at most newest with
// its bit set, keeps the parts outside, counts what it cleared, and rejoins
// neighbours that now agree, so a piece split only by its bit becomes one
// entry again.
func TestUnmark(t *testing.T) {
	// Below the piece's version nothing is cleared: the unmark names content
	// older than what it covers, and clearing a newer bit would hide a commit.
	t.Run("window", func(t *testing.T) {
		var s heldSet
		a, b := rec(1, 0), rec(2, 0)
		s.insert(write(0, 4, a, 3))
		s.insert(write(8, 4, b, 5))
		s.markOffloaded(ext(0, 4), ver(3), ver(3))
		if n := s.unmark(ext(0, 16), ver(2)); n != 0 {
			t.Fatalf("unmark below the versions cleared %d parts, want 0", n)
		}
		if n := s.unmark(ext(0, 16), ver(5)); n != 1 {
			t.Fatalf("unmark cleared %d parts, want the v3 piece only", n)
		}
		assertPieces(t, &s,
			Piece{Extent: ext(0, 4), Record: a, Version: ver(3)},
			Piece{Extent: ext(8, 4), Record: b, Version: ver(5)},
		)
	})

	// A piece split only by its bit joins again once the marked part is
	// cleared, so it costs one entry, as before the mark. An unmark that left
	// the two parts would cost the index a second entry forever.
	t.Run("rejoins neighbours", func(t *testing.T) {
		var s heldSet
		a := rec(1, 0)
		s.insert(write(0, 8, a, 1))
		s.markOffloaded(ext(4, 4), ver(1), ver(1))
		if s.len() != 2 {
			t.Fatalf("a mark inside one record left %d entries, want 2", s.len())
		}
		if n := s.unmark(ext(4, 4), ver(1)); n != 1 {
			t.Fatalf("unmark cleared %d parts, want the marked one", n)
		}
		assertPieces(t, &s, Piece{Extent: ext(0, 8), Record: a, Version: ver(1)})
	})

	// An already-clear bit is not counted and a gap clears nothing.
	t.Run("idempotent and gaps", func(t *testing.T) {
		var s heldSet
		s.insert(write(0, 4, rec(1, 0), 1))
		s.unmark(ext(0, 4), ver(1))
		if n := s.unmark(ext(0, 4), ver(1)); n != 0 {
			t.Fatalf("unmark of nothing reported %d parts", n)
		}
		if n := s.unmark(ext(100, 4), ver(9)); n != 0 {
			t.Fatalf("unmark of a gap reported %d parts", n)
		}
		assertPieces(t, &s, Piece{Extent: ext(0, 4), Record: rec(1, 0), Version: ver(1)})
	})
}
