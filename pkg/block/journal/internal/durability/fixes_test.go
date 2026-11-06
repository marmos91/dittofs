package durability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// strand appends a record of f to a segment, fails that segment's seal so it
// ends, and appends a second record of g, which rolls to a new segment. It
// returns the two locations.
func (r *rig) strand(t *testing.T, f, g core.FileID) (older, newer segment.Location) {
	t.Helper()
	a := r.appendWrite(t, f, 0, "a")
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, errors.New("seal boom"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal was expected to fail")
	}
	b := r.appendWrite(t, g, 0, "b")
	if b.Segment == a.Segment {
		t.Fatal("the append did not roll to a new segment")
	}
	return a, b
}

// TestEndedOlderSegmentDoesNotSpinTheTimer pins that a record of an ended
// older segment, which no sync of the active segment can cover, does not make
// the timer fire back to back: with the clock frozen, the active segment's
// sync count must stay flat.
func TestEndedOlderSegmentDoesNotSpinTheTimer(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	_, b := r.strand(t, file(1), file(2))
	r.clock.Advance(core.SyncBound)
	time.Sleep(50 * time.Millisecond)
	c1 := r.syncsOf(b.Segment)
	time.Sleep(50 * time.Millisecond)
	if c2 := r.syncsOf(b.Segment); c2 != c1 || c2 > 3 {
		t.Fatalf("syncs of the active segment went %d then %d with the clock frozen", c1, c2)
	}
}

// TestSyncOverARecordStrandedInAnOlderSegmentIsNotNil pins that Sync names
// every segment a file has an uncovered record in: the later record's sync
// does not make the earlier, stranded one durable, and a waiter on it never
// leads a sync of the active segment.
func TestSyncOverARecordStrandedInAnOlderSegmentIsNotNil(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a, b := r.strand(t, f, f)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := r.d.Sync(ctx, f)
	if err == nil {
		t.Fatal("Sync returned nil over a record that is not durable")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync = %v, want the context's error", err)
	}
	if r.d.Durable(a) {
		t.Fatal("the stranded record reads durable")
	}
	if got := r.syncsOf(b.Segment); got > 2 {
		t.Errorf("a waiter on the older segment issued %d syncs of the active one", got)
	}
}

// TestFailureIsRecordedAgainstTheSegmentSynced pins that a failed sync lands
// on the segment it was a sync of even when a seal and a roll run before the
// worker takes its lock: the waiter on the failed segment gets the SyncError
// and the new segment is not poisoned.
func TestFailureIsRecordedAgainstTheSegmentSynced(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	f, g := file(1), file(2)
	a := r.appendWrite(t, f, 0, "a")
	boom := errors.New("sync boom")
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, boom)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	r.d.afterSync = func() {
		once.Do(func() {
			_, _ = r.streams.For(f).Seal(false)
			r.appendWrite(t, g, 0, "b")
		})
	}
	r.d.Start(r.streams)

	err := mustReturn(t, "Sync of the failed segment", r.syncAsync(context.Background(), f))
	var se *core.SyncError
	if !errors.As(err, &se) || se.Segment != a.Segment {
		t.Fatalf("Sync = %v, want a SyncError of segment %d", err, a.Segment)
	}
	if err := mustReturn(t, "Sync of the new segment", r.syncAsync(context.Background(), g)); err != nil {
		t.Fatalf("Sync of a record in a segment that never failed = %v", err)
	}
}

// TestClosedStreamIsNotAFailedSync pins that a Sync over a closed stream
// answers core.ErrClosed alone, with no failure counted or recorded.
func TestClosedStreamIsNotAFailedSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	r.appendWrite(t, f, 0, "a")
	if err := r.streams.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f))
	if !errors.Is(err, core.ErrClosed) || errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("Sync over a closed stream = %v, want ErrClosed and not ErrSyncFailed", err)
	}
	if got := r.d.Stats().SyncFailures; got != 0 {
		t.Errorf("SyncFailures = %d, want 0", got)
	}
}
