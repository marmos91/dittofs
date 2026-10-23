package model

import (
	"context"
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
		ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
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
	ok(t, m.Compare(t.Context(), shareA, modelView{kept, shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 5, 1, false}, {5, 10, 2, false}})

	m = setup(t)
	ok(t, m.Compare(t.Context(), shareA, modelView{lost, shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, false}})

	m = setup(t)
	diverged(t, m.Compare(t.Context(), shareA, modelView{neither, shareA}))
}

// A write split at a segment cap is one record per segment, each surviving a
// crash whole or not at all, so the write can come back partly held. Its bytes
// are still its own: a state holding other bytes in its place is refused.
func TestCrashMayKeepPartOfASplitWrite(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
		ok(t, m.Crash())
		ok(t, m.Reopen(ver(10)))
		return m
	}

	torn := New()
	ok(t, torn.WriteAt(shareA, fid(1), 0, fill('A', 6), 0, ver(1), nil))
	m := setup(t)
	ok(t, m.Compare(t.Context(), shareA, modelView{torn, shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 6, 1, false}})

	other := New()
	ok(t, other.WriteAt(shareA, fid(1), 0, fill('B', 6), 0, ver(1), nil))
	diverged(t, setup(t).Compare(t.Context(), shareA, modelView{other, shareA}))
	// The pieces of a split write survive in order: a later piece's segment
	// exists only once the earlier one's was sealed and synced. So a write
	// never comes back as a suffix of itself, or with a hole inside.
	suffix := New()
	ok(t, suffix.WriteAt(shareA, fid(1), 4, fill('A', 6), 0, ver(1), nil))
	diverged(t, setup(t).Compare(t.Context(), shareA, modelView{suffix, shareA}))

	hole := New()
	ok(t, hole.WriteAt(shareA, fid(1), 0, fill('A', 3), 0, ver(1), nil))
	ok(t, hole.WriteAt(shareA, fid(1), 6, fill('A', 4), 0, ver(2), nil))
	hole.files[fid(1)].recs[1].ver = ver(1) // the same write's bytes, after a hole
	diverged(t, setup(t).Compare(t.Context(), shareA, modelView{hole, shareA}))
}

// Bytes a removal outranks look the same kept or lost, so they never break
// the order: a write whose first bytes a durable removal hides may come back
// with only its later bytes showing.
func TestBytesHiddenByARemovalDoNotBreakTheOrder(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 4), ver(2), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	j := New()
	ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, j.Deallocate(shareA, fid(1), ext(0, 4), ver(2), nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
}

// A removal is durable on return, so a crash never undoes it, synced or not.
func TestCrashNeverUndoesARemoval(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Truncate(shareA, fid(1), 4, ver(2), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	undone := New()
	ok(t, undone.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	diverged(t, m.Compare(t.Context(), shareA, modelView{undone, shareA}))
}

// Settling is not persisted, so a marker the caller settled before a crash
// may come back after it, and the caller settles it again; repack may also
// have dropped its record, so it may not. An unsettled marker must come back.
func TestCrashBringsBackMarkers(t *testing.T) {
	setup := func(t *testing.T, settle bool) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
		ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
		ok(t, m.Deallocate(shareA, fid(1), ext(0, 2), ver(2), nil))
		if settle {
			ok(t, m.Settle(shareA, fid(1), ver(2)))
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
			ok(t, j.Settle(shareA, fid(1), ver(2)))
		}
		return j
	}

	m := setup(t, true)
	ok(t, m.Compare(t.Context(), shareA, modelView{journal(false), shareA}))
	if got := since(t, m, shareA, fid(1), ver(1)); len(got) != 1 {
		t.Fatalf("a marker that came back is not live in the model: %s", fmtChanges(got))
	}

	m = setup(t, true)
	ok(t, m.Compare(t.Context(), shareA, modelView{journal(true), shareA}))

	m = setup(t, false)
	diverged(t, m.Compare(t.Context(), shareA, modelView{journal(true), shareA}))
}

// An offloaded record lost to a crash leaves the bit unset: wrong only in the
// safe direction, a redundant offload.
func TestCrashMayLoseAnUnsyncedMark(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 10, 1))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	ok(t, m.EndOffer(shareA, fid(1), journalv2.Version{}, nil, nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	dirtyAgain := New()
	ok(t, dirtyAgain.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{dirtyAgain, shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, false}})
}

// After a reopen every version the journal assigns exceeds the floor it was
// opened with, so none issued before the crash is issued again.
func TestVersionsAfterReopenExceedTheFloor(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 10), 0, ver(2), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(5)))

	lost := New()
	ok(t, lost.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{lost, shareA}))
	diverged(t, m.WriteAt(shareA, fid(1), 0, fill('C', 1), 0, ver(2), nil))
	diverged(t, m.WriteAt(shareA, fid(1), 0, fill('C', 1), 0, ver(5), nil))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('C', 1), 0, ver(6), nil))
}

// Between a crash and its resolution the model takes no call on the file: the
// state it would apply the call to is not known yet.
func TestNoCallBetweenACrashAndItsResolution(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Crash())
	misused(t, m.WriteAt(shareA, fid(2), 0, fill('A', 1), 0, ver(2), nil))
	ok(t, m.Reopen(ver(10)))
	misused(t, m.WriteAt(shareA, fid(1), 0, fill('A', 1), 0, ver(11), nil))
	ok(t, m.WriteAt(shareA, fid(2), 0, fill('A', 1), 0, ver(11), nil))
}

// The states a crash allows double with each unsynced record the journal's
// bytes cannot decide, so the model refuses to search past its bound rather
// than run for hours, and changes nothing when it refuses. Writes are decided
// from the bytes, so any number of them is accepted.
func TestCrashRefusesTooManyUndecidableRecordsInOneFile(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 20), 0, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))))
	for i := range maxUncertain + 1 {
		ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(int64(i), 1)}, nil))
	}
	ok(t, m.EndOffer(shareA, fid(1), journalv2.Version{}, nil, nil))
	misused(t, m.Crash())
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 1), 0, ver(2), nil))

	w := New()
	for i := range maxUncertain + 1 {
		ok(t, w.WriteAt(shareA, fid(1), int64(i), []byte{byte(i)}, 0, ver(uint64(i)+1), nil))
	}
	ok(t, w.Crash())
}

// A record that survived a crash is on disk: a second crash cannot lose it.
func TestARecordThatSurvivedACrashIsDurable(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	kept := New()
	ok(t, kept.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{kept, shareA}))

	ok(t, m.Crash())
	ok(t, m.Reopen(ver(20)))
	diverged(t, m.Compare(t.Context(), shareA, modelView{New(), shareA}))
}

// The floor of a reopen carries the version limit, so the counter restarts
// above every version issued before the crash: a lower floor is refused.
func TestReopenRefusesAFloorBelowAVersionIssued(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(7), nil))
	ok(t, m.Crash())
	misused(t, m.Reopen(ver(6)))
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
	ok(t, m.Compare(t.Context(), shareA, modelView{New(), shareA}))
	equalSpans(t, heldSpans(t, m, fid(1)), nil)
}

// Once a removal's settled marker did not come back, repack may have dropped
// its record, and the change version the journal rebuilds need not count it.
func TestChangeVersionNeedNotCountAMarkerThatDidNotComeBack(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(20, 2), ver(2), nil))
	ok(t, m.Settle(shareA, fid(1), ver(2)))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	j := New()
	ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
	ok(t, m.Fill(shareA, fid(1), 30, fill('F', 2), ver(1), ver(1), nil))
}

// A fill may carry the version of a write it refills. Where a release and the
// refill of synced, offloaded content are both lost, the journal serves the
// write again, and its bytes at that version are the write's, not the fill's.
func TestCrashTellsAFillFromTheWriteItRefills(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 7, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 10, 1))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	ok(t, m.EndOffer(shareA, fid(1), journalv2.Version{}, nil, nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('A', 10), ver(1), ver(1), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	j := New()
	ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 10), 7, ver(1), nil))
	ok(t, j.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(j, fid(1), 1, 1, dirty(0, 10, 1))))
	ok(t, j.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	ok(t, j.EndOffer(shareA, fid(1), journalv2.Version{}, nil, nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
}

// A fill's version is the ref's, not one the journal issued, so it bounds no
// reopen floor. Once the fill is lost, versions above the floor are issued
// again; while it is held, every version issued exceeds it.
func TestAFillVersionBoundsTheCounterOnlyWhileHeld(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(5), nil))
		ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
		ok(t, m.Fill(shareA, fid(1), 100, fill('F', 4), ver(5), ver(50), nil))
		ok(t, m.Crash())
		ok(t, m.Reopen(ver(10)))
		return m
	}
	write := New()
	ok(t, write.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(5), nil))

	m := setup(t)
	ok(t, m.Compare(t.Context(), shareA, modelView{write, shareA}))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 1), 0, ver(11), nil))

	both := New()
	ok(t, both.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(5), nil))
	ok(t, both.Fill(shareA, fid(1), 100, fill('F', 4), ver(5), ver(50), nil))
	m = setup(t)
	ok(t, m.Compare(t.Context(), shareA, modelView{both, shareA}))
	diverged(t, m.WriteAt(shareA, fid(1), 0, fill('B', 1), 0, ver(11), nil))
}

// A settled marker that did not come back after one crash never comes back
// after the next: repack dropped its record.
func TestAMarkerThatDidNotComeBackNeverDoes(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 2), ver(2), nil))
	ok(t, m.Settle(shareA, fid(1), ver(2)))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	gone := New()
	ok(t, gone.WriteAt(shareA, fid(1), 2, fill('A', 8), 0, ver(1), nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{gone, shareA}))

	ok(t, m.Crash())
	ok(t, m.Reopen(ver(20)))
	back := New()
	ok(t, back.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, back.Deallocate(shareA, fid(1), ext(0, 2), ver(2), nil))
	diverged(t, m.Compare(t.Context(), shareA, modelView{back, shareA}))
}

// A write hidden under a removal whose marker did not come back backs nothing,
// so after a crash it no longer holds the change version up, whether it was
// kept or lost: the two states look the same, and both are accepted.
func TestAHiddenWriteCannotBeToldApartAfterACrash(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
		ok(t, m.Deallocate(shareA, fid(1), ext(0, 4), ver(2), nil))
		ok(t, m.Settle(shareA, fid(1), ver(2)))
		ok(t, m.Crash())
		ok(t, m.Reopen(ver(10)))
		return m
	}
	journal := func(writeKept bool) *Model {
		j := setup(t)
		f := j.files[fid(1)]
		items := f.items() // the write, then the settled marker
		mask := uint32(0)
		if writeKept {
			mask = 1
		}
		candidate{items: items, mask: mask}.adopt(f)
		return j
	}

	for _, kept := range []bool{true, false} {
		m, j := setup(t), journal(kept)
		ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
		if got, want := m.current(fid(1)).change, j.current(fid(1)).change; got != want {
			t.Fatalf("write kept %t: model change version %s, journal %s", kept, fmtVersion(got), fmtVersion(want))
		}
	}
}

// asOfView is a journal view that reports one change version for every read.
type asOfView struct {
	modelView
	asOf journalv2.Version
}

func (v asOfView) ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	n, missing, held, _, err := v.modelView.ReadAt(ctx, id, off, p)
	return n, missing, held, v.asOf, err
}

// Overlapping unsynced writes are decided from the bytes alone: the one the
// journal serves is kept, and every write under it is hidden for good, so no
// search over them is needed however many there are.
func TestOverlappingUnsyncedWritesNeedNoSearch(t *testing.T) {
	m := New()
	for i := 1; i <= 20; i++ {
		ok(t, m.WriteAt(shareA, fid(1), 0, fill(byte('a'+i), 4), 0, ver(uint64(i)), nil))
	}
	j := clone(m)
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(30)))
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
}

// Every version the journal issues exceeds every version on disk, so once it
// issued v50 after a reopen, a fill at v100 that the crash left uncertain
// cannot have survived.
func TestAVersionIssuedAfterAReopenBoundsWhatSurvived(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareB, fid(2), 0, fill('x', 1), 0, ver(1), nil))
		ok(t, m.Sync(shareB, []journalv2.FileID{fid(2)}, nil))
		ok(t, m.Fill(shareA, fid(1), 0, fill('f', 4), journalv2.Version{}, ver(100), nil))
		ok(t, m.Crash())
		ok(t, m.Reopen(ver(5)))
		ok(t, m.WriteAt(shareB, fid(2), 0, fill('y', 1), 0, ver(50), nil))
		return m
	}
	kept := New()
	ok(t, kept.Fill(shareA, fid(1), 0, fill('f', 4), journalv2.Version{}, ver(100), nil))
	diverged(t, setup(t).Compare(t.Context(), shareA, modelView{kept, shareA}))
	ok(t, setup(t).Compare(t.Context(), shareA, modelView{New(), shareA}))
}

// A marker that came back after a crash still counts in the file's change
// version: a journal reporting an older one is refused.
func TestAMarkerThatCameBackCountsInTheChangeVersion(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 2), ver(2), nil))
	ok(t, m.Settle(shareA, fid(1), ver(2)))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	j := New()
	ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, j.Deallocate(shareA, fid(1), ext(0, 2), ver(2), nil))
	diverged(t, m.Compare(t.Context(), shareA, asOfView{modelView{j, shareA}, ver(1)}))
}

// Repack may drop a record that backs nothing, and recovery rebuilds a file's
// change version from the records on disk, so after a crash a released write
// no longer holds the change version up.
func TestAfterACrashTheChangeVersionCountsOnlyLiveRecords(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('a', 4), 0, ver(1), nil))
	ok(t, m.WriteAt(shareA, fid(1), 4, fill('b', 4), 0, ver(2), nil))
	ok(t, m.Offer(shareA, fid(1), ver(2), 0, 0, offerOf(m, fid(1), 1, 2, dirty(0, 4, 1), dirty(4, 4, 2))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(4, 4)}, nil))
	ok(t, m.EndOffer(shareA, fid(1), journalv2.Version{}, nil, nil))
	ok(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(4, 4)}, nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	j := clone(m)
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	ok(t, m.Compare(t.Context(), shareA, asOfView{modelView{j, shareA}, ver(1)}))
}

// A Compare that finds no state the crash allows leaves the model as it was.
func TestAFailedCompareChangesNothing(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	before := len(m.files[fid(1)].recs)
	other := New()
	ok(t, other.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
	ok(t, other.WriteAt(shareA, fid(1), 4, fill('B', 2), 0, ver(2), nil))
	diverged(t, m.Compare(t.Context(), shareA, modelView{other, shareA}))
	if after := len(m.files[fid(1)].recs); after != before {
		t.Fatalf("a failed Compare left %d records, not %d", after, before)
	}
}

// A floor equal to the last version issued is high enough: the counter
// restarts above it.
func TestReopenAcceptsAFloorAtTheLastVersionIssued(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(7), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(7)))
}

// A write that a lost newer write had hidden is live again once the crash is
// resolved, and holds the change version up: a fill as of an older version is
// refused.
func TestAWriteLiveAgainAfterACrashCountsInTheChangeVersion(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 4), 0, ver(2), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	j := New()
	ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 4), 0, ver(1), nil))
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
	diverged(t, m.Fill(shareA, fid(1), 10, fill('F', 2), journalv2.Version{}, ver(1), nil))
}

// A refill at a released version is several records like any write, and may
// come back as a prefix of itself: where it was lost, the content it replaced
// is served at the same version, with other bytes.
func TestCrashMayKeepPartOfASameVersionRefill(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(2), 0, fill('x', 1), 0, ver(5), nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('V', 4), ver(5), ver(5), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(0, 3)}, nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('E', 3), ver(5), ver(5), nil))
	j := clone(m)
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	f := j.files[fid(1)]
	var recs []*record
	for _, r := range f.recs {
		switch {
		case r.kind == kindRelease: // lost
		case r.kind == kindFill && r.data[0] == 'E':
			recs = append(recs, r.piece(0, 2)) // only its first piece kept
		default:
			recs = append(recs, r)
		}
	}
	f.recs = recs
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
}

// An unsettled marker comes back after a crash, and its removal still holds
// the change version up: a journal reporting an older one is refused.
func TestAnUnsettledMarkerCountsInTheChangeVersionAfterACrash(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Truncate(shareA, fid(1), 4, ver(2), nil))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))

	j := New()
	ok(t, j.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, j.Truncate(shareA, fid(1), 4, ver(2), nil))
	diverged(t, m.Compare(t.Context(), shareA, asOfView{modelView{j, shareA}, ver(1)}))
}

// The bound a version issued after a crash sets on what survived it holds
// across a second crash, and it holds at that version too: no version issued
// equals one on disk.
func TestAVersionIssuedAfterACrashBoundsItsRecordsAcrossReopens(t *testing.T) {
	setup := func(t *testing.T, issue uint64, again bool, later ...uint64) *Model {
		m := New()
		ok(t, m.WriteAt(shareB, fid(2), 0, fill('x', 1), 0, ver(1), nil))
		ok(t, m.Sync(shareB, []journalv2.FileID{fid(2)}, nil))
		ok(t, m.Fill(shareA, fid(1), 0, fill('f', 4), journalv2.Version{}, ver(50), nil))
		ok(t, m.Crash())
		ok(t, m.Reopen(ver(5)))
		ok(t, m.WriteAt(shareB, fid(2), 0, fill('y', 1), 0, ver(issue), nil))
		for _, v := range later {
			ok(t, m.WriteAt(shareB, fid(2), 0, fill('z', 1), 0, ver(v), nil))
		}
		if again {
			ok(t, m.Sync(shareB, []journalv2.FileID{fid(2)}, nil))
			ok(t, m.Crash())
			ok(t, m.Reopen(ver(issue)))
		}
		return m
	}
	kept := New()
	ok(t, kept.Fill(shareA, fid(1), 0, fill('f', 4), journalv2.Version{}, ver(50), nil))
	diverged(t, setup(t, 50, false).Compare(t.Context(), shareA, modelView{kept, shareA}))
	diverged(t, setup(t, 20, true).Compare(t.Context(), shareA, modelView{kept, shareA}))
	diverged(t, setup(t, 20, false, 60).Compare(t.Context(), shareA, modelView{kept, shareA}))
	ok(t, setup(t, 20, true).Compare(t.Context(), shareA, modelView{New(), shareA}))
}

// Sync, Settle and Since are refused on a file whose state a crash left
// unknown, as every other call is, and a Sync error the model has no rule for
// is reported.
func TestQueriesAndSettlesAreRefusedUntilACrashIsResolved(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	diverged(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, journalv2.ErrCorrupt))
	ok(t, m.Crash())
	misused(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	ok(t, m.Reopen(ver(10)))
	misused(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
	misused(t, m.Settle(shareA, fid(1), ver(1)))
	_, err := m.Since(shareA, fid(1), journalv2.Version{})
	misused(t, err)
}

// Writes are decided from the bytes the journal shows, however many there are:
// kept where it serves them, lost where it serves an older version or nothing.
// Each case has more writes than the search could take.
func TestWritesAreDecidedFromTheBytesWithoutTheSearch(t *testing.T) {
	const n = maxUncertain + 1
	cases := []struct {
		name    string
		synced  bool // an older synced write under each
		journal func(j *Model)
	}{
		{"all kept", false, func(j *Model) {
			for i := range n {
				ok(t, j.WriteAt(shareA, fid(1), int64(i)*4, fill(byte('a'+i), 4), 0, ver(uint64(i)+10), nil))
			}
		}},
		{"all lost over nothing", false, func(j *Model) {}},
		{"all lost over an older version", true, func(j *Model) {
			ok(t, j.WriteAt(shareA, fid(1), 0, fill('0', 4*n), 0, ver(1), nil))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New()
			if c.synced {
				ok(t, m.WriteAt(shareA, fid(1), 0, fill('0', 4*n), 0, ver(1), nil))
				ok(t, m.Sync(shareA, []journalv2.FileID{fid(1)}, nil))
			}
			for i := range n {
				ok(t, m.WriteAt(shareA, fid(1), int64(i)*4, fill(byte('a'+i), 4), 0, ver(uint64(i)+10), nil))
			}
			ok(t, m.Crash())
			ok(t, m.Reopen(ver(100)))
			j := New()
			c.journal(j)
			ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
		})
	}
}

// Bytes a newer write outranks look the same kept or lost, so they never break
// the order: an older write whose first bytes a newer one covers comes back
// with only its later bytes showing.
func TestBytesUnderANewerWriteDoNotBreakTheOrder(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 4), 0, ver(2), nil))
	j := clone(m)
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
}

// Bytes a later record of the same version serves look the same kept or lost:
// a fill whose first bytes a refill at its version replaced comes back with
// only its later bytes showing.
func TestBytesUnderALaterRecordOfTheVersionDoNotBreakTheOrder(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(2), 0, fill('x', 1), 0, ver(5), nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('V', 4), ver(5), ver(5), nil))
	ok(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(0, 2)}, nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('E', 2), ver(5), ver(5), nil))
	j := clone(m)
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	ok(t, m.Compare(t.Context(), shareA, modelView{j, shareA}))
}
