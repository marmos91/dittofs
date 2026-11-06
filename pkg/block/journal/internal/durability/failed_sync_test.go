package durability

import (
	"context"
	"errors"
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

// failSyncOf schedules one failing sync of the segment's file at the seam.
func (r *rig) failSyncOf(t *testing.T, seg core.SegmentID, err error) {
	t.Helper()
	if e := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(seg), 1, err)); e != nil {
		t.Fatal(e)
	}
}

// predecessorOf reads the predecessor the segment's header names.
func (r *rig) predecessorOf(t *testing.T, seg core.SegmentID) core.SegmentID {
	t.Helper()
	f, err := r.seam.Open(format.SegmentName(seg))
	if err != nil {
		t.Fatalf("opening segment %d: %v", seg, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, format.SegmentHeaderSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("reading the header of segment %d: %v", seg, err)
	}
	h, err := format.DecodeSegmentHeader(buf, rigIdentity)
	if err != nil {
		t.Fatalf("decoding the header of segment %d: %v", seg, err)
	}
	return h.Predecessor
}

// fileOn returns a file that lives on stream i.
func (r *rig) fileOn(t *testing.T, i int) core.FileID {
	t.Helper()
	for b := 1; b < 255; b++ {
		if f := file(byte(b)); r.streams.For(f) == r.streams.Stream(i) {
			return f
		}
	}
	t.Fatalf("no file maps to stream %d", i)
	return core.FileID{}
}

// TestFailedSyncEndsItsSegmentAndTheNextAppendRollsToAFreshOne covers R13: the
// failed segment is ended and never sealed, and the stream continues in a
// segment whose header names no predecessor.
func TestFailedSyncEndsItsSegmentAndTheNextAppendRollsToAFreshOne(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	r.failSyncOf(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); !errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("Sync = %v, want ErrSyncFailed", err)
	}
	eventually(t, "the failed segment to end", func() bool {
		st, err := r.set.State(a.Segment)
		return err == nil && st == segment.Ended
	})

	b := r.appendWrite(t, file(2), 0, "b")
	if b.Segment == a.Segment {
		t.Fatal("the append after a failed sync stayed in the failed segment")
	}
	if got := r.predecessorOf(t, b.Segment); got != 0 {
		t.Errorf("the new segment's header names predecessor %d, want none", got)
	}
	if st, err := r.set.State(a.Segment); err != nil || st != segment.Ended {
		t.Errorf("the failed segment's state = %v, %v, want Ended", st, err)
	}
}

// TestSyncSucceedsInTheSegmentAfterAFailedOne covers R73: a write made after
// the failure syncs in the new segment and its Sync returns nil.
func TestSyncSucceedsInTheSegmentAfterAFailedOne(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f, g := file(1), file(2)
	a := r.appendWrite(t, f, 0, "a")
	r.failSyncOf(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); !errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("Sync = %v, want ErrSyncFailed", err)
	}
	b := r.appendWrite(t, g, 0, "b")
	if b.Segment == a.Segment {
		t.Fatal("the append did not roll")
	}
	if err := mustReturn(t, "Sync of the new segment", r.syncAsync(context.Background(), g)); err != nil {
		t.Fatalf("Sync after the failure = %v, want nil", err)
	}
}

// TestStatsReportTheLastSyncFailure covers R62 and R64: the counter and the
// most recent failure with its time, the later one winning across streams.
func TestStatsReportTheLastSyncFailure(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 2)
	r.d.keepWindows.Store(true)
	if got := r.d.Stats().LastSyncError; got != nil {
		t.Fatalf("LastSyncError before any failure = %+v", got)
	}
	f0, f1 := r.fileOn(t, 0), r.fileOn(t, 1)
	first, second := errors.New("first boom"), errors.New("second boom")

	// Each failure is scheduled before the clock moves, since the move may
	// itself fire the sync bound's sync.
	a0 := r.appendWrite(t, f0, 0, "a")
	r.failSyncOf(t, a0.Segment, first)
	r.clock.Advance(time.Second)
	_ = mustReturn(t, "Sync of stream 0", r.syncAsync(context.Background(), f0))
	eventually(t, "the first failure to be counted", func() bool { return r.d.Stats().SyncFailures == 1 })
	got := r.d.Stats().LastSyncError
	if got == nil || !errors.Is(got.Err, first) {
		t.Fatalf("LastSyncError = %+v, want the first failure", got)
	}
	if want := r.clock.Now(); !got.At.Equal(want) {
		t.Errorf("LastSyncError.At = %v, want the clock's %v", got.At, want)
	}

	a1 := r.appendWrite(t, f1, 0, "b")
	r.failSyncOf(t, a1.Segment, second)
	r.clock.Advance(time.Second)
	_ = mustReturn(t, "Sync of stream 1", r.syncAsync(context.Background(), f1))
	eventually(t, "the second failure to be counted", func() bool { return r.d.Stats().SyncFailures == 2 })
	got = r.d.Stats().LastSyncError
	if got == nil || !errors.Is(got.Err, second) {
		t.Fatalf("LastSyncError = %+v, want the later failure", got)
	}
	if want := r.clock.Now(); !got.At.Equal(want) {
		t.Errorf("LastSyncError.At = %v, want the clock's %v", got.At, want)
	}
}

// TestASyncOfAnEndedSegmentIsNotAFailure covers the worker's handling of
// ErrSegmentEnded: a segment a failed append ended answers the sync with the
// refusal, which counts nothing and is not issued again for it. A slot still
// writing holds the segment's end event back, so the waiter leads before the
// event is taken, and fails once the event delivers the window.
func TestASyncOfAnEndedSegmentIsNotAFailure(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	r.d.keepWindows.Store(true)
	var attempts atomic.Int64
	r.d.afterSync = func() { attempts.Add(1) }
	r.d.Start(r.streams)
	f, g := file(1), file(2)
	a := r.appendWrite(t, f, 0, "a")

	hold := r.seam.HoldFirst(t, journaltest.OpWrite, format.SegmentName(a.Segment))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h := format.RecordHeader{Kind: format.KindWrite, File: g, Extent: core.Extent{Len: 1}, Version: core.Version{Counter: 1}, Tag: 1}
		_, _ = r.streams.For(g).Append(h, format.RecordBody{Payload: []byte("g"), MTime: 1})
	}()
	<-hold.Reached()
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpWrite, format.SegmentName(a.Segment), 1, errors.New("write boom"))); err != nil {
		t.Fatal(err)
	}
	r.appendFailing(t, f, 1)

	ch := r.syncAsync(context.Background(), f)
	st := r.d.made[0]
	eventually(t, "the worker to find the segment ended", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		_, ok := st.ending[a.Segment]
		return ok
	})
	mustBlock(t, "Sync over an ended segment", ch)
	// The sync bound passes while the event is held back: the timer must not
	// issue the sync the segment refuses.
	for range 3 {
		r.clock.Advance(2 * time.Second)
		mustBlock(t, "Sync over an ended segment", ch)
	}
	if got := r.syncsOf(a.Segment); got != 0 {
		t.Errorf("the seam saw %d syncs of the ended segment, want 0", got)
	}

	hold.Release()
	wg.Wait()
	wantSyncError(t, mustReturn(t, "Sync once the window is taken", ch), a.Segment, f)
	if got := attempts.Load(); got != 1 {
		t.Errorf("the worker issued %d syncs of the ended segment, want 1", got)
	}
	if got := r.d.Stats(); got.SyncFailures != 0 || got.LastSyncError != nil {
		t.Errorf("an ended segment's refusal was counted: %+v", got)
	}
}
