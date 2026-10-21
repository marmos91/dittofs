package model

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// Fill writes only the sub-extents that are absent, never overwrites a held
// one, and sets the offloaded bit of what it filled.
func TestFillWritesOnlyAbsentPartsAndSetsTheBit(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 10, fill('W', 10), 0, ver(5), nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('F', 30), ver(5), ver(3), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 10, 3, true},
		{10, 10, 5, false},
		{20, 10, 3, true},
	})
	if got := string(heldBytes(t, m, fid(1), 8, 4)); got != "FFWW" {
		t.Fatalf("bytes 8..12 = %q, want %q", got, "FFWW")
	}
}

// A fill older than the file is refused: when the file's change version is
// newer than asOf, Fill must write nothing and return ErrStaleFill.
func TestFillOlderThanTheFileMustBeRefused(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('W', 4), 0, ver(5), nil))

	refused(t, m.Fill(shareA, fid(1), 10, fill('F', 4), ver(4), ver(2), nil))
	ok(t, m.Fill(shareA, fid(1), 10, fill('F', 4), ver(4), ver(2), journalv2.ErrStaleFill))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 4, 5, false}})
}

// A fill whose asOf is at or above every version the journal has issued
// cannot be refused as stale: no file and no floor bucket is newer.
func TestFillAsOfEveryVersionMustBeAccepted(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('W', 4), 0, ver(5), nil))
	refused(t, m.Fill(shareA, fid(1), 10, fill('F', 4), ver(5), ver(5), journalv2.ErrStaleFill))
}

// Floor buckets are coarse: a fill of a file unchanged since asOf may still be
// refused when other files have newer versions, and may be accepted.
func TestFillMayBeRefusedByACoarseBucket(t *testing.T) {
	for _, err := range []error{nil, journalv2.ErrStaleFill} {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('W', 4), 0, ver(5), nil))
		ok(t, m.WriteAt(shareA, fid(2), 0, fill('X', 4), 0, ver(9), nil))
		ok(t, m.Fill(shareA, fid(1), 10, fill('F', 4), ver(5), ver(2), err))
	}
}

// Content with a version below a removal's is not held inside the removed
// extent, so a fill below a removal's version is excluded.
func TestFillBelowARemovalsVersionIsExcluded(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('W', 10), 0, ver(1), nil))
	ok(t, m.Truncate(shareA, fid(1), 0, ver(2), nil))

	ok(t, m.Fill(shareA, fid(1), 0, fill('F', 10), ver(2), ver(1), nil))
	equalSpans(t, heldSpans(t, m, fid(1)), nil)
}

// A fill carries the version of the ref it was fetched from, never one below a
// release or a settled removal over its bytes: repack may have dropped such a
// record, which the model cannot see, so it does not decide such a fill.
func TestFillBelowADroppableRecordIsOutsideTheModel(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 10), ver(2), nil))
	ok(t, m.Settle(shareA, fid(1), ver(2)))
	refused(t, m.Fill(shareA, fid(1), 0, fill('F', 4), ver(2), ver(1), nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('F', 4), ver(2), ver(2), nil))

	// Below a release: the release leaves no marker.
	r := New()
	ok(t, r.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(5), nil))
	ok(t, r.Offer(shareA, fid(1), ver(5), 0, 0, offerOf(r, fid(1), 5, 5, dirty(0, 10, 5))))
	ok(t, r.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	r.EndOffer(shareA, fid(1))
	ok(t, r.Release(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	refused(t, r.Fill(shareA, fid(1), 0, fill('F', 4), ver(5), ver(4), nil))

	// Below a removal whose settled marker did not come back after a crash.
	g := New()
	ok(t, g.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, g.Sync(shareA, nil, fid(1)))
	ok(t, g.Deallocate(shareA, fid(1), ext(0, 10), ver(2), nil))
	ok(t, g.Settle(shareA, fid(1), ver(2)))
	ok(t, g.Crash())
	ok(t, g.Reopen(ver(10)))
	ok(t, g.Compare(shareA, modelView{New(), shareA}))
	ok(t, g.WriteAt(shareA, fid(2), 0, fill('x', 1), 0, ver(11), nil))
	refused(t, g.Fill(shareA, fid(1), 0, fill('F', 4), ver(11), ver(1), nil))
}

// A fill below a removal whose marker is not settled is decided: the marker is
// live, its record stays on disk, and content below its version is not held
// inside it.
func TestFillBelowAnUnsettledMarkerIsDecided(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 10), ver(2), nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('F', 4), ver(2), ver(1), nil))
	equalSpans(t, heldSpans(t, m, fid(1)), nil)
}
