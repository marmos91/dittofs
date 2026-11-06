package durability

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// appendTagged appends one write record of the file under a share tag.
func (r *rig) appendTagged(t *testing.T, f core.FileID, tag core.ShareTag, off int64, payload string) segment.Location {
	t.Helper()
	h := format.RecordHeader{
		Kind:    format.KindWrite,
		File:    f,
		Extent:  core.Extent{Off: off, Len: int64(len(payload))},
		Version: core.Version{Counter: uint64(off + 1)},
		Tag:     tag,
	}
	p, err := r.streams.For(f).Append(h, format.RecordBody{Payload: []byte(payload), MTime: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return p.Loc
}

// armed waits until the worker has stamped the records and armed its timer,
// which shows as one more pending wait on the clock than before.
func (r *rig) armed(t *testing.T, before int) {
	t.Helper()
	eventually(t, "the sync timer to arm", func() bool { return r.clock.Pending() > before })
}

// quiet fails if a sync of the segment reaches the seam within a short real
// interval; the clock is frozen, so nothing but a bug can issue one.
func (r *rig) quiet(t *testing.T, seg core.SegmentID, want int) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	if got := r.syncsOf(seg); got != want {
		t.Fatalf("%d syncs of the segment, want %d", got, want)
	}
}

// TestBoundSyncsExactlyAtTheDeadline covers: a record nobody syncs is
// synced when SyncBound has passed since the worker saw it, not a tick sooner.
func TestBoundSyncsExactlyAtTheDeadline(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	a := r.appendWrite(t, file(1), 0, "a")
	r.armed(t, before)
	base := r.syncsOf(a.Segment)

	r.clock.Advance(core.SyncBound - time.Nanosecond)
	r.quiet(t, a.Segment, base)
	if r.d.Durable(a) {
		t.Fatal("the record is durable one tick before the bound")
	}
	r.clock.Advance(time.Nanosecond)
	eventually(t, "the bound's sync", func() bool { return r.d.Durable(a) })
	if got := r.syncsOf(a.Segment) - base; got != 1 {
		t.Errorf("%d syncs at the bound, want 1", got)
	}
}

// TestOneTimerSyncCoversEveryRecordBeforeTheFire pins that the timer is per
// stream: two records appended before the fire cost one sync.
func TestOneTimerSyncCoversEveryRecordBeforeTheFire(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	a := r.appendWrite(t, file(1), 0, "a")
	r.armed(t, before)
	r.clock.Advance(time.Second / 2)
	b := r.appendWrite(t, file(2), 0, "b")
	base := r.syncsOf(a.Segment)

	r.clock.Advance(core.SyncBound - time.Second/2)
	eventually(t, "both records to be durable", func() bool { return r.d.Durable(a) && r.d.Durable(b) })
	if got := r.syncsOf(a.Segment) - base; got != 1 {
		t.Errorf("%d syncs for two records, want 1", got)
	}
}

// TestTimerIsPerStreamNotPerShare covers: records of two share tags on one
// stream are covered by one timer sync, so no per-share mode exists.
func TestTimerIsPerStreamNotPerShare(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	a := r.appendTagged(t, file(1), 1, 0, "a")
	b := r.appendTagged(t, file(2), 2, 0, "b")
	r.armed(t, before)
	base := r.syncsOf(a.Segment)

	r.clock.Advance(core.SyncBound)
	eventually(t, "both shares' records to be durable", func() bool { return r.d.Durable(a) && r.d.Durable(b) })
	if got := r.syncsOf(a.Segment) - base; got != 1 {
		t.Errorf("%d syncs for two shares on one stream, want 1", got)
	}
}

// TestFireWithNothingUnsyncedIssuesNoSync pins that the timer syncs only when
// a record is uncovered: an explicit Sync that covered the record before the
// deadline leaves the tick with nothing to do, and a later record gets its
// own full bound.
func TestFireWithNothingUnsyncedIssuesNoSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	a := r.appendWrite(t, file(1), 0, "a")
	r.armed(t, before)
	if err := r.d.Sync(context.Background(), file(1)); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	base := r.syncsOf(a.Segment)

	r.clock.Advance(core.SyncBound)
	r.quiet(t, a.Segment, base)

	// The record appended now is stamped after the fire, so a stale timer
	// must not sync it early.
	mid := r.clock.Pending()
	b := r.appendWrite(t, file(1), 8, "b")
	r.armed(t, mid)
	r.clock.Advance(core.SyncBound - time.Nanosecond)
	r.quiet(t, a.Segment, base)
	r.clock.Advance(time.Nanosecond)
	eventually(t, "the later record's sync", func() bool { return r.d.Durable(b) })
	if got := r.syncsOf(a.Segment) - base; got != 1 {
		t.Errorf("%d syncs after the fire, want 1", got)
	}
}

// TestFailedSegmentIsNotRetriedByTheTimer covers: the
// failed segment's records stay uncovered, and a fire must not sync it again
// every second.
func TestFailedSegmentIsNotRetriedByTheTimer(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	before := r.clock.Pending()
	a := r.appendWrite(t, file(1), 0, "a")
	r.armed(t, before)
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, context.DeadlineExceeded)); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(core.SyncBound)
	eventually(t, "the failed sync", func() bool { return r.d.Stats().SyncFailures == 1 })
	base := r.syncsOf(a.Segment)
	for range 3 {
		r.clock.Advance(core.SyncBound)
		r.quiet(t, a.Segment, base)
	}
}

// TestStopWithTimerArmedAndWaiterParkedLeaksNothing pins that Stop joins the
// worker and releases a parked waiter while a timer is outstanding.
func TestStopWithTimerArmedAndWaiterParkedLeaksNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	before := r.clock.Pending()
	a := r.appendWrite(t, file(1), 0, "a")
	r.armed(t, before)
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))
	ch := make(chan error, 1)
	go func() { ch <- r.d.WaitDurable(a) }()
	<-h.Reached()
	stopped := make(chan struct{})
	go func() { r.d.Stop(); close(stopped) }()
	if err := mustReturn(t, "parked WaitDurable", ch); err != core.ErrClosed {
		t.Fatalf("parked WaitDurable after Stop = %v, want ErrClosed", err)
	}
	h.Release()
	<-stopped
}
