package durability

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// stamped waits until the worker has stamped every entry of the stream.
func (r *rig) stamped(t *testing.T, no int) {
	t.Helper()
	st := r.d.made[uint32(no)]
	eventually(t, "the worker to stamp the ledger", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		for i := range st.ledger {
			if !st.ledger[i].seenSet {
				return false
			}
		}
		return true
	})
}

// TestStatsTakesNoLockAWriteNeeds pins that Stats is served while a stream's
// leaf mutex, which every append takes, is held.
func TestStatsTakesNoLockAWriteNeeds(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	st := r.d.made[0]
	st.mu.Lock()
	done := make(chan Part, 1)
	go func() { done <- r.d.Stats() }()
	select {
	case <-done:
		st.mu.Unlock()
	case <-time.After(5 * time.Second):
		st.mu.Unlock()
		t.Fatal("Stats blocked on the stream's mutex")
	}
}

// TestRearmKeepsTheBoundFromTheRecordsOwnStamp pins that a fire which finds
// the oldest record not yet due arms for that record's own deadline, not a
// whole bound from the fire.
func TestRearmKeepsTheBoundFromTheRecordsOwnStamp(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	a := r.appendWrite(t, file(1), 0, "a")
	r.armed(t, before)
	r.clock.Advance(core.SyncBound / 2)
	if err := r.d.Sync(context.Background(), file(1)); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	r.clock.Advance(core.SyncBound / 10)
	b := r.appendWrite(t, file(2), 0, "b")
	r.stamped(t, 0)

	// The timer armed for a fires with only b left, which is not due.
	r.clock.Advance(core.SyncBound - core.SyncBound/2 - core.SyncBound/10)
	time.Sleep(30 * time.Millisecond)
	if r.d.Durable(b) {
		t.Fatal("b is durable before its bound")
	}
	// b was stamped at 0.6 bound, so it is due at 1.6 bound.
	r.clock.Advance(core.SyncBound / 2 * 6 / 5)
	eventually(t, "b to be synced at its own deadline", func() bool { return r.d.Durable(b) })
	_ = a
}

// TestOneTimerIsOutstandingPerStream pins that appends before a fire arm one
// wait on the clock, not one each.
func TestOneTimerIsOutstandingPerStream(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	for i := range 20 {
		r.appendWrite(t, file(1), int64(i*8), "x")
	}
	r.armed(t, before)
	r.stamped(t, 0)
	time.Sleep(30 * time.Millisecond)
	if got := r.clock.Pending() - before; got != 1 {
		t.Fatalf("%d waits pending on the clock after 20 appends, want 1", got)
	}
}

// TestSyncDurationOnTheBoundFallsInTheLowerBucket pins that a bound is
// inclusive: a sync of exactly the first bound counts in the first bucket.
func TestSyncDurationOnTheBoundFallsInTheLowerBucket(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	a := r.appendWrite(t, file(1), 0, "a")
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))
	ch := r.syncAsync(context.Background(), file(1))
	<-h.Reached()
	r.clock.Advance(SyncDurationBounds[0])
	h.Release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	eventually(t, "the sync to be counted", func() bool { return r.d.Stats().Syncs == 1 })
	if c := r.d.Stats().SyncDuration.Counts; c[0] != 1 {
		t.Fatalf("buckets = %v, want the sync of exactly %v in bucket 0", c, SyncDurationBounds[0])
	}
}

// TestCompletionRunsOnlyAfterASyncReports pins that a pass with no sync
// reported since the last one does not rescan the ledger: an entry whose
// segment's offset moved without the sink hearing of it stays, and the sink's
// report completes it.
func TestCompletionRunsOnlyAfterASyncReports(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	a := r.appendWrite(t, file(1), 0, "a")
	r.armed(t, before)
	r.stamped(t, 0)
	st := r.d.made[0]
	st.mu.Lock()
	st.synced[a.Segment] = a.Off + a.Len
	st.mu.Unlock()
	st.wake()
	time.Sleep(30 * time.Millisecond)
	if n := r.ledgerLen(0); n != 1 {
		t.Fatalf("ledger holds %d entries, want 1: a pass rescanned with nothing reported", n)
	}
	r.syncStream(t, file(1))
	eventually(t, "the report to complete the entry", func() bool { return r.ledgerLen(0) == 0 })
}
