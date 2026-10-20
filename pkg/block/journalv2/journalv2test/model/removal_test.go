package model

import (
	"math"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// Truncate stops holding every extent at or beyond size, narrows the one
// straddling it, and leaves a marker over [size, end of file).
func TestTruncateNarrowsTheStraddlingExtentAndLeavesAMarker(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 7, ver(1), nil))
	ok(t, m.WriteAt(shareA, fid(1), 20, fill('B', 10), 7, ver(2), nil))
	ok(t, m.Truncate(shareA, fid(1), 5, ver(3), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 5, 1, false}})
	equalChanges(t, m.Since(shareA, fid(1), ver(2)), []journalv2.Change{
		{Kind: journalv2.ChangeTruncate, Extent: ext(5, math.MaxInt64-5), Version: ver(3)},
	})
}

// A write after a truncate, at a newer version, is held again over the
// truncated range: content at or above the marker's version is held.
func TestWriteAfterTruncateIsHeld(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Truncate(shareA, fid(1), 0, ver(2), nil))
	ok(t, m.WriteAt(shareA, fid(1), 4, fill('B', 2), 0, ver(3), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{{4, 2, 3, false}})
}

// Deallocate stops holding its extent, whatever the offloaded bit: inside one
// extent it splits it, across several it trims both ends.
func TestDeallocateInsideAndAcrossExtents(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.WriteAt(shareA, fid(1), 10, fill('B', 10), 0, ver(2), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(2, 2), ver(3), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(8, 4), ver(4), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 2, 1, false},
		{4, 4, 1, false},
		{12, 8, 2, false},
	})
}

// A zero-length deallocate removes nothing and is refused with ErrZeroLength.
func TestZeroLengthDeallocateIsRefused(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	refused(t, m.Deallocate(shareA, fid(1), ext(3, 0), ver(2), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(3, 0), journalv2.Version{}, journalv2.ErrZeroLength))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, false}})
}

// Delete stops holding every extent of the file and leaves one marker.
func TestDeleteDropsEveryExtent(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.WriteAt(shareA, fid(1), 100, fill('B', 10), 0, ver(2), nil))
	ok(t, m.Delete(shareA, fid(1), ver(3), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), nil)
	equalChanges(t, m.Since(shareA, fid(1), journalv2.Version{}), []journalv2.Change{
		{Kind: journalv2.ChangeDelete, Extent: ext(0, math.MaxInt64), Version: ver(3)},
	})
}

// A removal draws a version like a write: it must exceed every version before
// it.
func TestRemovalVersionMustExceedEveryVersionSeen(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(4), nil))
	refused(t, m.Truncate(shareA, fid(1), 0, ver(4), nil))
	refused(t, m.Deallocate(shareA, fid(1), ext(0, 1), ver(3), nil))
	refused(t, m.Delete(shareA, fid(1), ver(1), nil))
}

// Settle(v) drops the markers at or below v and keeps the newer ones.
func TestSettleDropsMarkersAtOrBelowItsVersion(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 1), ver(2), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(1, 1), ver(3), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(2, 1), ver(4), nil))
	m.Settle(shareA, fid(1), ver(3))

	equalChanges(t, m.Since(shareA, fid(1), ver(1)), []journalv2.Change{
		{Kind: journalv2.ChangeDeallocate, Extent: ext(2, 1), Version: ver(4)},
	})
}

// Since yields every held extent and unsettled marker strictly above from, in
// version order. A held extent is a write whether written or filled; a write
// carries its mtime and a filled extent none.
func TestSinceYieldsHeldExtentsAndMarkersInVersionOrder(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 111, ver(3), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(4, 2), ver(5), nil))
	ok(t, m.Fill(shareA, fid(1), 20, fill('F', 5), ver(5), ver(4), nil))
	ok(t, m.WriteAt(shareA, fid(1), 30, fill('C', 5), 222, ver(6), nil))

	equalChanges(t, m.Since(shareA, fid(1), ver(2)), []journalv2.Change{
		{Kind: journalv2.ChangeWrite, Extent: ext(0, 4), Version: ver(3), Mtime: 111},
		{Kind: journalv2.ChangeWrite, Extent: ext(6, 4), Version: ver(3), Mtime: 111},
		{Kind: journalv2.ChangeWrite, Extent: ext(20, 5), Version: ver(4)},
		{Kind: journalv2.ChangeDeallocate, Extent: ext(4, 2), Version: ver(5)},
		{Kind: journalv2.ChangeWrite, Extent: ext(30, 5), Version: ver(6), Mtime: 222},
	})
	equalChanges(t, m.Since(shareA, fid(1), ver(4)), []journalv2.Change{
		{Kind: journalv2.ChangeDeallocate, Extent: ext(4, 2), Version: ver(5)},
		{Kind: journalv2.ChangeWrite, Extent: ext(30, 5), Version: ver(6), Mtime: 222},
	})
}

func equalChanges(t *testing.T, got, want []journalv2.Change) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("changes:\n got %+v\nwant %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("change %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}
