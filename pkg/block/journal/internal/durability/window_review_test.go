package durability

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestASealThatFindsAFailedSyncDoesNotCountItAgain pins that one failed fsync
// is one sync failure even when a seal finds it and ends the segment with
// EndSeal before the worker's own End lands.
func TestASealThatFindsAFailedSyncDoesNotCountItAgain(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	f := file(1)
	var once sync.Once
	r.d.afterSync = func() { once.Do(func() { _, _ = r.streams.For(f).Seal(false) }) }
	r.d.Start(r.streams)
	a := r.appendWrite(t, f, 0, "a")
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, errors.New("sync boom"))); err != nil {
		t.Fatal(err)
	}
	first := mustReturn(t, "Sync", r.syncAsync(context.Background(), f))
	wantSyncError(t, first, a.Segment, f)
	eventually(t, "the window to be resolved", func() bool { return len(r.windowsOf(0)) == 0 })
	if got := r.d.Stats().SyncFailures; got != 1 {
		t.Errorf("one failed fsync counted %d times", got)
	}
	// The sync's own error stands: the seal's EndSeal carries another one.
	// The seal's error wraps ErrSegmentEnded and says a sync failed; the
	// sync's own does neither.
	ownError := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "sync boom") &&
			!errors.Is(err, segment.ErrSegmentEnded) && !strings.Contains(err.Error(), "a sync of the segment failed")
	}
	if se := r.d.Stats().LastSyncError; se == nil || !ownError(se.Err) {
		t.Errorf("LastSyncError = %+v, want the sync's own error", se)
	}
	var se *core.SyncError
	if !errors.As(first, &se) || !ownError(se.Err) {
		t.Errorf("SyncError.Err = %v, want the sync's own error", first)
	}
}

// TestALateSyncReportNeverSatisfiesAFailedWindow covers R14 at the layer that
// consumes the report: a Synced value for the failed segment, before and after
// its window is taken, satisfies none of the window's records.
func TestALateSyncReportNeverSatisfiesAFailedWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	f := file(1)
	var a segment.Location
	var once sync.Once
	// The late report lands while the window is pending with its failure
	// already recorded, and again once it is resolved.
	r.d.afterFail = func() {
		once.Do(func() { r.sinks[0].Synced(a.Segment, a.Off+a.Len) })
	}
	r.d.Start(r.streams)
	a = r.appendWrite(t, f, 0, "a")
	r.failSyncOf(t, a.Segment, errors.New("sync boom"))
	r.stamped(t, 0)
	eventually(t, "the sync bound to issue the failing sync", func() bool {
		r.clock.Advance(2 * core.SyncBound)
		return r.d.Stats().SyncFailures == 1
	})
	eventually(t, "the window to be resolved", func() bool {
		return r.d.Stats().SyncFailures == 1 && len(r.windowsOf(0)) == 0
	})
	r.sinks[0].Synced(a.Segment, a.Off+a.Len)
	if r.d.Durable(a) {
		t.Error("a record of the failed window reads durable after a late report")
	}
	wantSyncError(t, mustReturn(t, "Sync after a late report", r.syncAsync(context.Background(), f)), a.Segment, f)
	if r.d.Durable(a) {
		t.Error("a record of the failed window reads durable after its failure was reported")
	}
}

// TestAFailedWindowKeepsItsReportThroughACompletionPass pins that a later
// segment's sync, which completes the ledger, does not drop the report a
// failed window left: a Sync naming the failed file still answers.
func TestAFailedWindowKeepsItsReportThroughACompletionPass(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f, g := file(1), file(2)
	a := r.appendWrite(t, f, 0, "a")
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, g, 0)
	r.waitResolved(t, 0)
	r.appendWrite(t, g, 0, "b")
	if err := mustReturn(t, "Sync of the new segment", r.syncAsync(context.Background(), g)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ledger to complete", func() bool { return r.ledgerLen(0) == 0 })
	wantSyncError(t, mustReturn(t, "Sync of the failed file", r.syncAsync(context.Background(), f)), a.Segment, f)
}

// TestADurableRecordIsNotInTheWindow pins that a record a sync covered before
// the segment's end event was taken stays durable and out of the window, when
// the sync's report and the event reach the worker in one pass.
func TestADurableRecordIsNotInTheWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var got recorder[resolvedAt]
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(stream uint32, seg core.SegmentID) { got.add(resolvedAt{stream, seg}) }
	})
	f, g := file(1), file(2)
	park, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.d.afterSync = func() {
		once.Do(func() {
			close(park)
			<-release
		})
	}
	r.d.Start(r.streams)
	a := r.appendWrite(t, f, 0, "a")
	ch := r.syncAsync(context.Background(), f)
	<-park
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, g, 0)
	close(release)
	// The window is empty, so it is announced resolved when it is taken.
	eventually(t, "the segment's window to be taken", func() bool { return len(got.all()) == 1 })
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync of a record a sync covered = %v", err)
	}
	if !r.d.Durable(a) {
		t.Error("a record a sync covered reads not durable")
	}
	if n := len(r.windowsOf(0)); n != 0 {
		t.Errorf("%d windows pending, want none", n)
	}
}

// TestAnEndingMarkIsNotLeftByAStaleWant pins that a sync the worker issues from
// a want that predates the segment's end event leaves no ending mark behind.
//
// The want is planted only once the worker has armed its timer, the last step
// of the pass a's Track woke: a worker still in that pass would take the want
// at once, sync a's segment and cover a, so the window would be empty and
// resolved as soon as it was taken. Planted while the worker is idle, with no
// wake, the want is first read in the pass the end event wakes, after that
// pass took the window.
func TestAnEndingMarkIsNotLeftByAStaleWant(t *testing.T) {
	prof.Watch(t, prof.Options{})
	resolved := make(chan core.SegmentID, 8)
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(_ uint32, seg core.SegmentID) { resolved <- seg }
	})
	r.d.Start(r.streams)
	f, g := file(1), file(2)
	before := r.clock.Pending()
	a := r.appendWrite(t, f, 0, "a")
	r.armed(t, before)
	st := r.d.made[0]
	st.mu.Lock()
	st.want = true
	st.mu.Unlock()
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, g, 0)
	// The stale want's sync may cover the record and leave an empty window,
	// which is announced as well, so the announcement is the wait.
	select {
	case <-resolved:
	case <-time.After(5 * time.Second):
		t.Fatal("the segment's window was never resolved")
	}
	r.appendWrite(t, g, 0, "b")
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), g)); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := len(st.ending); n != 0 {
		t.Errorf("%d ending marks left for segments whose event was taken", n)
	}
}

// TestAnEndingMarkOfAResolvedSegmentIsPrunedOnCompletion pins that a mark left
// for a segment no entry or window names any longer (an empty window resolved
// and dropped its failure, then a stale want marked the segment again) is
// removed by the next completion that covers a record.
func TestAnEndingMarkOfAResolvedSegmentIsPrunedOnCompletion(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	r.stamped(t, 0)
	st := r.d.made[0]
	gone := a.Segment + 1000
	st.mu.Lock()
	st.ending[gone] = struct{}{}
	st.mu.Unlock()
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ledger to complete", func() bool { return r.ledgerLen(0) == 0 })
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.ending[gone]; ok {
		t.Error("ending mark of a segment nothing names was left after a completion")
	}
}

// TestRemovingAnOlderSegmentsEntriesKeepsTheStampedPrefixExact pins the
// ledger compaction the window and the completion pass share: after entries
// leave from before the stamped boundary, stamped counts exactly the stamped
// entries that stay, so the first unstamped one is the one the timer reads.
func TestRemovingAnOlderSegmentsEntriesKeepsTheStampedPrefixExact(t *testing.T) {
	prof.Watch(t, prof.Options{})
	at := func(seg core.SegmentID) entry {
		return entry{w: segment.Written{Loc: segment.Location{Segment: seg}}}
	}
	// Segment 1 holds two stamped entries, segment 2 one stamped and one not.
	st := &stream{ledger: []entry{at(1), at(2), at(1), at(2)}, stamped: 3}
	var taken int
	st.removeLocked(func(e *entry) bool { return e.w.Loc.Segment == 1 }, func(entry) { taken++ })
	if taken != 2 || len(st.ledger) != 2 {
		t.Fatalf("took %d entries and kept %d, want 2 and 2", taken, len(st.ledger))
	}
	if st.stamped != 1 {
		t.Errorf("stamped = %d after the older segment left, want 1: only one kept entry was stamped", st.stamped)
	}
}

// TestASealEndAfterTheFailureWasPrunedDoesNotCountItAgain pins that one failed
// fsync is one sync failure when the completion pass has already dropped the
// segment's failure before the seal's EndSeal is taken: the dedupe cannot lean
// on the failure map alone. The end event is held back by holding d.mu, which
// onEnd takes, so the order is deterministic: failed sync, seal ends the
// segment first, a later segment's completion prunes the failure, event taken.
func TestASealEndAfterTheFailureWasPrunedDoesNotCountItAgain(t *testing.T) {
	prof.Watch(t, prof.Options{})
	resolved := make(chan core.SegmentID, 8)
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(_ uint32, seg core.SegmentID) { resolved <- seg }
	})
	f := file(1)
	var once sync.Once
	var armed, sealed atomic.Bool
	r.d.afterSync = func() {
		if !armed.Load() {
			return
		}
		once.Do(func() {
			_, _ = r.streams.For(f).Seal(false)
			sealed.Store(true)
		})
	}
	r.d.Start(r.streams)
	st := r.d.made[0]
	a := r.appendWrite(t, f, 0, "a")
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ledger to complete", func() bool { return r.ledgerLen(0) == 0 })
	// Hold the end event back: onEnd needs d.mu.
	r.d.mu.Lock()
	held := true
	defer func() {
		if held {
			r.d.mu.Unlock()
		}
	}()
	armed.Store(true)
	// One failure: the worker's sync. The seal that follows finds the failed
	// sync and ends the segment without an fsync of its own.
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, errors.New("sync boom"))); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.want = true
	st.mu.Unlock()
	st.wake()
	eventually(t, "the failed sync to be counted", func() bool { return st.failures.Load() == 1 })
	if !sealed.Load() {
		t.Fatal("the seal never ran")
	}
	// A record of the next segment completes: the pass prunes the failure of
	// the segment nothing names.
	b := r.appendWrite(t, f, 0, "b")
	if b.Segment == a.Segment {
		t.Fatalf("the append stayed in segment %d", a.Segment)
	}
	st.mu.Lock()
	st.want = true
	st.mu.Unlock()
	st.wake()
	eventually(t, "the failure to be pruned", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		_, kept := st.failed[a.Segment]
		return !kept && len(st.ledger) == 0
	})
	r.d.mu.Unlock()
	held = false
	select {
	case seg := <-resolved:
		if seg != a.Segment {
			t.Fatalf("resolved segment %d, want %d", seg, a.Segment)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the seal's end was never taken")
	}
	if got := r.d.Stats().SyncFailures; got != 1 {
		t.Errorf("one failed fsync counted %d times", got)
	}
}
