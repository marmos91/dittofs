package segment

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journal/internal/format"
	"github.com/marmos91/dittofs/pkg/block/journal/internal/platform"
	"github.com/marmos91/dittofs/pkg/block/journal/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest/prof"
)

// streamSyncDoneOf records what a returned Sync covered, so the D2 checks
// drive the tracker's sync half without a reached-for Sync.
func streamSyncDoneOf(s *Stream, through int64) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.tracker.syncDone(through)
}

// streamAppendLockHeld reports whether the stream's append lock is held, for
// the check that no seam call runs under it: an I/O call made under the lock

// streamTrackerView is the tracker's state as the D2 checks read it: the
// well-completed prefix, the synced value the next reserved record carries,

// streamTracker reads the tracker under the append lock, the one domain that

// streamTrackerReserve reserves a slot through the stream's own tracker, so
// the allocation check exercises the production path exactly.
func streamTrackerReserve(s *Stream, off, end int64) uint64 {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.tracker.reserve(off, end)
}

// streamTrackerComplete completes a slot a probe reserved, marking it well
// when ok.
func streamTrackerComplete(s *Stream, slot uint64, ok bool) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.tracker.complete(slot, ok)
}

// streamWarmTheRing brings the tracker's ring to steady state: the warm-up
// the allocation check runs first, so the count it takes is of a ring it
// never has to grow.
func streamWarmTheRing(s *Stream, slots int) {
	for i := range slots {
		streamTrackerReserve(s, 48+int64(i)*16, 48+int64(i+1)*16)
		streamTrackerComplete(s, uint64(i), true)
	}
}

// streamSortedPositions returns the record offsets of the stream's catalog

// streamClosed reports whether Close has set the stream's closed flag, for

// streamSyncedThroughOf returns the synced-through value the next reserved
// record would carry.
func streamSyncedThroughOf(s *Stream) int64 {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.tracker.synced
}

// streamAppendLockHeldForCheck is the alias stream_test.go's held-fault

// TestStreamTrackerALaterSlotNeverCarriesTheEarlierOnesMissingSync checks the
// ordered-completion prefix (D2): reserve two slots, complete the second and
// sync — the value the next record carries must not move past the first
// slot's offset — then complete the first and sync again — the value covers
// both. It catches a "last synced offset", which would have covered the
// in-flight first slot's bytes at the first sync.
func TestStreamTrackerALaterSlotNeverCarriesTheEarlierOnesMissingSync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := streamProbeStream(t)
	defer func() { _ = s.Close() }()

	a := streamTrackerReserve(s, 200, 300)
	b := streamTrackerReserve(s, 300, 400)
	streamTrackerComplete(s, b, true)
	if got := streamSyncedThroughOf(s); got != 0 {
		t.Fatalf("after a sync-word covering only the completed prefix, the tracker reports %d, want 0: the in-flight slot bounds it", got)
	}

	// Completing a lets the prefix take b in, so the next sync word covers
	// both.
	streamTrackerComplete(s, a, true)
	streamSyncDoneOf(s, 400)
	if got := streamSyncedThroughOf(s); got != 400 {
		t.Fatalf("after both slots completed and a sync, the tracker reports %d, want 400", got)
	}
}

// TestStreamTrackerAFailedSlotFreezesThePrefix checks the frozen half of D2:
// a slot whose write failed stops the prefix at its own offset, and no
// completion or sync after the failure moves it — so a later record's
// synced-through stays below the failed slot's, and no record ever claims
// durability past bytes no fsync reached.
func TestStreamTrackerAFailedSlotFreezesThePrefix(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := streamProbeStream(t)
	defer func() { _ = s.Close() }()

	a := streamTrackerReserve(s, 100, 200) // the one that fails
	b := streamTrackerReserve(s, 200, 300)
	streamTrackerComplete(s, a, false)
	streamTrackerComplete(s, b, true)

	for i := 0; i < 3; i++ {
		streamSyncDoneOf(s, 300)
		if got := streamSyncedThroughOf(s); got > 100 {
			t.Fatalf("after a failed slot and %d more syncs, the tracker reports %d, want it frozen at or below 100", i+1, got)
		}
		c := streamTrackerReserve(s, 300, 400)
		streamTrackerComplete(s, c, true)
		if got := streamSyncedThroughOf(s); got > 100 {
			t.Fatalf("a completion after the failure moved the tracker to %d, want the prefix frozen", got)
		}
	}
}

// TestStreamReservationAllocatesNothing checks the reservation's cost: after
// a warm-up grows the ring, a reserve and a complete together allocate
// nothing, so a stream of appends never touches the allocator on its locked
// path. The race detector instruments the locks, so the count is taken only
// without it.
func TestStreamReservationAllocatesNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector instruments the locks; the allocation count is taken without it")
	}
	prof.Watch(t, prof.Options{})

	s := streamProbeStream(t)
	defer func() { _ = s.Close() }()

	streamWarmTheRing(s, 1<<7)

	var sink uint64
	allocs := testing.AllocsPerRun(200, func() {
		slot := streamTrackerReserve(s, 512, 528)
		streamTrackerComplete(s, slot, true)
		sink = slot
	})
	_ = sink
	if allocs != 0 {
		t.Fatalf("a reserve and a complete allocated %v per round, want 0", allocs)
	}
}

// streamProbeStream builds a stream whose tracker alone is exercised: the
// dependencies are real — a set, a maker and a cache over a real directory —
// but nothing appends, because the checks below drive the tracker directly.
func streamProbeStream(t *testing.T) *Stream {
	t.Helper()

	dir := t.TempDir()
	set := NewSet(64<<10, nil)
	maker := NewMaker(probeFS(set, dir), set, 1<<62, 0, nil)
	cache := NewDescriptors(probeFS(set, dir), format.SegmentName, core.NewDescriptorBudget(64))
	return NewStream(1, probeIdentity, set, maker, cache)
}

// probeIdentity is the identity the probe stream writes under.
var probeIdentity = core.JournalID{0xd, 0xe, 0xc, 0xa, 0xd, 0x0}

// probeFS is a nil-safe wrapper: the probe stream takes no segment, so any
// storage is unreachable; a nil filesystem would still be accepted (the
// Descriptors and the Maker panic on it), so this hands them a real one over
// the temp directory.
func probeFS(_ *Set, dir string) storage.FS { return platform.NewOS(dir) }
