package model

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// A synced write survives a crash; an unsynced write is held whole or not at
// all, and where it is lost, the content it superseded is held again. A state
// that loses the synced write is refused.
func TestCrashKeepsSyncedWritesAndAllowsUnsyncedOnesEitherWay(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
		m.Sync(shareA, fid(1))
		ok(t, m.WriteAt(shareA, fid(1), 5, fill('B', 10), 0, ver(2), nil))
		ok(t, m.Crash())
		ok(t, m.Reopen(ver(10)))
		return m
	}

	kept := New()
	ok(t, kept.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, kept.WriteAt(shareA, fid(1), 5, fill('B', 10), 0, ver(2), nil))
	lost := New()
	ok(t, lost.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	neither := New()

	m := setup(t)
	ok(t, m.Compare(shareA, modelView{kept, shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 5, 1, false}, {5, 10, 2, false}})

	m = setup(t)
	ok(t, m.Compare(shareA, modelView{lost, shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, false}})

	m = setup(t)
	refused(t, m.Compare(shareA, modelView{neither, shareA}))
}

// A write lost to a crash is lost whole: a state holding part of it is
// refused.
func TestCrashNeverKeepsPartOfAWrite(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	torn := New()
	ok(t, torn.WriteAt(shareA, fid(1), 0, fill('A', 6), 0, ver(1), nil))
	refused(t, m.Compare(shareA, modelView{torn, shareA}))
}

// A removal is durable on return, so a crash never undoes it, synced or not.
func TestCrashNeverUndoesARemoval(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	m.Sync(shareA, fid(1))
	ok(t, m.Truncate(shareA, fid(1), 4, ver(2), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	undone := New()
	ok(t, undone.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	refused(t, m.Compare(shareA, modelView{undone, shareA}))
}

// Settling is not persisted, so a marker the caller settled before a crash
// may come back after it, and the caller settles it again; repack may also
// have dropped its record, so it may not. An unsettled marker must come back.
func TestCrashBringsBackMarkers(t *testing.T) {
	setup := func(t *testing.T, settle bool) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
		m.Sync(shareA, fid(1))
		ok(t, m.Deallocate(shareA, fid(1), ext(0, 2), ver(2), nil))
		if settle {
			m.Settle(shareA, fid(1), ver(2))
		}
		ok(t, m.Crash())
		ok(t, m.Reopen(ver(10)))
		return m
	}
	journal := func(settled bool) *Model {
		j := New()
		ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
		ok(t, j.Deallocate(shareA, fid(1), ext(0, 2), ver(2), nil))
		if settled {
			j.Settle(shareA, fid(1), ver(2))
		}
		return j
	}

	m := setup(t, true)
	ok(t, m.Compare(shareA, modelView{journal(false), shareA}))
	if got := m.Since(shareA, fid(1), ver(1)); len(got) != 1 {
		t.Fatalf("a marker that came back is not live in the model: %+v", got)
	}

	m = setup(t, true)
	ok(t, m.Compare(shareA, modelView{journal(true), shareA}))

	m = setup(t, false)
	refused(t, m.Compare(shareA, modelView{journal(true), shareA}))
}

// An offloaded record lost to a crash leaves the bit unset: wrong only in the
// safe direction, a redundant offload.
func TestCrashMayLoseAnUnsyncedMark(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	m.Sync(shareA, fid(1))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 10, 1))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	m.EndOffer(shareA, fid(1))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	dirtyAgain := New()
	ok(t, dirtyAgain.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(shareA, modelView{dirtyAgain, shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, false}})
}

// After a reopen every version the journal assigns exceeds the floor it was
// opened with, so none issued before the crash is issued again.
func TestVersionsAfterReopenExceedTheFloor(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	m.Sync(shareA, fid(1))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 10), 0, ver(2), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(5)))

	lost := New()
	ok(t, lost.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(shareA, modelView{lost, shareA}))
	refused(t, m.WriteAt(shareA, fid(1), 0, fill('C', 1), 0, ver(2), nil))
	refused(t, m.WriteAt(shareA, fid(1), 0, fill('C', 1), 0, ver(5), nil))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('C', 1), 0, ver(6), nil))
}

// Between a crash and its resolution the model takes no call on the file: the
// state it would apply the call to is not known yet.
func TestNoCallBetweenACrashAndItsResolution(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Crash())
	refused(t, m.WriteAt(shareA, fid(2), 0, fill('A', 1), 0, ver(2), nil))
	ok(t, m.Reopen(ver(10)))
	refused(t, m.WriteAt(shareA, fid(1), 0, fill('A', 1), 0, ver(11), nil))
	ok(t, m.WriteAt(shareA, fid(2), 0, fill('A', 1), 0, ver(11), nil))
}

// The states a crash allows double with each unsynced record, so the model
// refuses to search past its bound rather than run for hours.
func TestCrashRefusesTooManyUnsyncedRecordsInOneFile(t *testing.T) {
	m := New()
	for i := range maxUncertain + 1 {
		ok(t, m.WriteAt(shareA, fid(1), int64(i), []byte{byte(i)}, 0, ver(uint64(i)+1), nil))
	}
	err := m.Crash()
	if err == nil {
		t.Fatalf("Crash accepted %d unsynced records in one file", maxUncertain+1)
	}
}

// A record that survived a crash is on disk: a second crash cannot lose it.
func TestARecordThatSurvivedACrashIsDurable(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	kept := New()
	ok(t, kept.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(shareA, modelView{kept, shareA}))

	ok(t, m.Crash())
	ok(t, m.Reopen(ver(20)))
	refused(t, m.Compare(shareA, modelView{New(), shareA}))
}

// The floor of a reopen carries the version limit, so the counter restarts
// above every version issued before the crash: a lower floor is refused.
func TestReopenRefusesAFloorBelowAVersionIssued(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(7), nil))
	ok(t, m.Crash())
	refused(t, m.Reopen(ver(6)))
}

// The search for the state a crash left is decided by what the journal shows
// wherever it can be: sixteen lost 4 KiB writes are decided record by record,
// not by trying every subset of them.
func TestCrashSearchIsDecidedByWhatTheJournalShows(t *testing.T) {
	m := New()
	for i := range maxUncertain {
		ok(t, m.WriteAt(shareA, fid(1), int64(i)<<12, fill(byte('a'+i), 4<<10), 0, ver(uint64(i)+1), nil))
	}
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(100)))
	ok(t, m.Compare(shareA, modelView{New(), shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), nil)
}

// Once a removal's settled marker did not come back, repack may have dropped
// its record, and the change version the journal rebuilds need not count it.
func TestChangeVersionNeedNotCountAMarkerThatDidNotComeBack(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	m.Sync(shareA, fid(1))
	ok(t, m.Deallocate(shareA, fid(1), ext(20, 2), ver(2), nil))
	m.Settle(shareA, fid(1), ver(2))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	j := New()
	ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(shareA, modelView{j, shareA}))
	ok(t, m.Fill(shareA, fid(1), 30, fill('F', 2), ver(1), ver(1), nil))
}
