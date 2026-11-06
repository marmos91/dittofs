package segment

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// StreamAppendLockHeld reports whether the stream's append lock is held, for
// the check that no storage call runs under it: an I/O call made under the
// lock is the segment append domain held across storage (R12).
func StreamAppendLockHeld(s *Stream) bool {
	if s.appendMu.TryLock() {
		s.appendMu.Unlock()
		return false
	}
	return true
}

// streamSyncStartOf reads the offset a Sync that starts now will cover once
// it returns: the well-completed prefix, the value Stream.Sync records when
// its fsync comes back.
func streamSyncStartOf(s *Stream) int64 {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.tracker.syncStart()
}

// streamSyncDoneOf records that a returned Sync covered up to through, the
// way Stream.Sync records the prefix its fsync covered.
func streamSyncDoneOf(s *Stream, through int64) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.tracker.syncDone(through)
}

// streamTrackerReserve reserves a slot through the stream's own tracker, so
// the checks exercise the production path exactly.
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

// streamTrackerLiveOf reports how many in-flight slots the tracker holds, so
// the freeze check asserts that the reservations after a failure are handed
// out but not held.
func streamTrackerLiveOf(s *Stream) int {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.tracker.live
}

// streamWarmTheRing brings the tracker to steady state: the warm-up the
// allocation check runs first, so the count it takes is of a slice it never
// has to grow.
func streamWarmTheRing(s *Stream, slots int) {
	for i := range slots {
		streamTrackerReserve(s, 48+int64(i)*16, 48+int64(i+1)*16)
		streamTrackerComplete(s, uint64(i), true)
	}
}

// streamSyncedThroughOf returns the synced-through value the next reserved
// record would carry.
func streamSyncedThroughOf(s *Stream) int64 {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.tracker.synced
}

// TestStreamTrackerALaterSlotNeverCarriesTheEarlierOnesMissingSync checks the
// ordered-completion prefix (D2): reserve two slots, complete the second and
// record the sync the way Stream.Sync does — through syncStart — so the value
// the next record carries must stay below the in-flight first slot; then
// complete the first, record again, and the value covers both. It catches a
// "last synced offset", which would have covered the in-flight slot's bytes
// at the first sync.
func TestStreamTrackerALaterSlotNeverCarriesTheEarlierOnesMissingSync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := streamProbeStream(t)
	defer func() { _ = s.Close() }()

	// Slot a is in flight; slot b completes and lands well.
	a := streamTrackerReserve(s, 200, 300)
	b := streamTrackerReserve(s, 300, 400)
	streamTrackerComplete(s, b, true)

	// A sync that starts now covers only the completed prefix, which the
	// in-flight a bounds: recorded the way Stream.Sync records it, the
	// value the next record carries stays at or below a's own offset.
	through := streamSyncStartOf(s)
	if through != 0 {
		t.Fatalf("the completed prefix is %d while slot a is in flight, want 0: nothing before the first slot has been completed", through)
	}
	streamSyncDoneOf(s, through)
	if got := streamSyncedThroughOf(s); got > 200 {
		t.Fatalf("after a sync word taken from the completed prefix, the tracker reports %d, want at most the in-flight slot's offset 200: a later slot's completion was counted", got)
	}

	// Completing a lets the prefix take b in, so the next recorded sync
	// covers both.
	streamTrackerComplete(s, a, true)
	streamSyncDoneOf(s, streamSyncStartOf(s))
	if got := streamSyncedThroughOf(s); got != 400 {
		t.Fatalf("after both slots completed and a recorded sync, the tracker reports %d, want the end of the second slot 400", got)
	}
}

// TestStreamTrackerAFailedSlotFreezesThePrefix checks the frozen half of D2:
// a slot whose write failed bounds the prefix at its own offset, and no
// completion or recorded sync after the failure moves it — so a later
// record's synced-through stays below the failed slot's, and no record ever
// claims durability past bytes no fsync reached. The reservations after the
// failure are handed out but not held, so the freeze bounds what the tracker
// carries to the two slots the failure found.
func TestStreamTrackerAFailedSlotFreezesThePrefix(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := streamProbeStream(t)
	defer func() { _ = s.Close() }()

	a := streamTrackerReserve(s, 200, 300) // the one that fails
	b := streamTrackerReserve(s, 300, 400)
	streamTrackerComplete(s, a, false)
	streamTrackerComplete(s, b, true)

	for i := 0; i < 3; i++ {
		c := streamTrackerReserve(s, 500, 600)
		streamTrackerComplete(s, c, true)

		// The reservations after the failure are handed out but not held:
		// the tracker carries only the two slots the failure found, not
		// one per later record.
		if got := streamTrackerLiveOf(s); got != 2 {
			t.Fatalf("after the failed slot and %d later reservations the tracker holds %d in-flight slots, want 2: the reservations after the freeze must not be enqueued", i+1, got)
		}

		// A sync recorded the way Stream.Sync records one moves neither
		// the prefix nor the value the next record carries past the
		// failed slot.
		streamSyncDoneOf(s, streamSyncStartOf(s))
		if got := streamSyncStartOf(s); got > 200 {
			t.Fatalf("after the failed slot and %d recorded syncs the completed prefix is %d, want it frozen at or below the failed slot's offset 200", i+1, got)
		}
		if got := streamSyncedThroughOf(s); got > 200 {
			t.Fatalf("after the failed slot and %d recorded syncs the tracker reports %d, want it frozen at or below the failed slot's offset 200", i+1, got)
		}
	}
}

// TestStreamReservationAllocatesNothing checks the reservation's cost: after
// a warm-up grows the in-flight slice, a reserve and a complete together
// allocate nothing, so a stream of appends never touches the allocator on its
// locked path. The race detector instruments the locks, so the count is taken
// only without it.
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

// StreamSetAppendParkHook parks every append that enters Append's loop at the
// loop top until fn returns — the one place an append already past the head
// checks can be held while another goroutine takes the lock, staged without
// handing the scheduler the race. The concurrency checks of the external test
// package reach the hook through this setter, for the hook lives in the
// stream's own file. A nil fn unparks every appender.
func StreamSetAppendParkHook(fn func()) { appendParkHook = fn }

// StreamSetNotifyParkHook parks the notifier at the top of its hand-over,
// before it waits, the same reach the append park holds: the install-wait
// check needs the notifier held between the ended segment's last completion
// and its event's snapshot. A nil fn unparks it.
func StreamSetNotifyParkHook(fn func()) { notifyParkHook = fn }
func streamProbeStream(t *testing.T) *Stream {
	t.Helper()

	dir := t.TempDir()
	set := NewSet(64<<10, nil)
	maker := NewMaker(probeFS(dir), set, 1<<62, 0, nil)
	pool := NewPool(maker, Targets{Content: 1}, nil, nil)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	cache := NewDescriptors(probeFS(dir), format.SegmentName, core.NewDescriptorBudget(64))
	return NewStream(1, probeIdentity, set, pool, cache, nopInternalSink())
}

// probeIdentity is the identity the probe stream writes under.
var probeIdentity = core.JournalID{0xd, 0xe, 0xc, 0xa, 0xd, 0x0}

// probeFS hands the probe stream's maker and cache a real filesystem over the
// temp directory: neither takes a nil one, though the checks above never
// reach storage through them.
func probeFS(dir string) storage.FS { return platform.NewOS(dir) }

// nopInternalSink is a durability sink that drops every event, for the
// internal tests that build a stream and care for neither report.
func nopInternalSink() Sink {
	return Sink{Track: func(Written) {}, Synced: func(core.SegmentID, int64) {}}
}
