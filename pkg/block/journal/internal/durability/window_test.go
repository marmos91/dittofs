package durability

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// windowView is a copy of a pending window, safe to read without the lock.
type windowView struct {
	seg   core.SegmentID
	cause segment.EndCause
	locs  []segment.Location
	files []core.FileID
}

// windowsOf copies stream no's pending windows.
func (r *rig) windowsOf(no int) []windowView {
	st := r.d.made[uint32(no)]
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []windowView
	for _, w := range st.windows {
		v := windowView{seg: w.seg, cause: w.cause}
		for _, e := range w.entries {
			v.locs = append(v.locs, e.w.Loc)
			v.files = append(v.files, e.w.File)
		}
		out = append(out, v)
	}
	return out
}

// waitWindow waits for stream no to hold a pending window and returns the
// first one.
func (r *rig) waitWindow(t *testing.T, no int) windowView {
	t.Helper()
	eventually(t, "the worker to take the window", func() bool { return len(r.windowsOf(no)) > 0 })
	return r.windowsOf(no)[0]
}

// failNextWrite schedules the next write of the segment's file to fail once.
func (r *rig) failNextWrite(t *testing.T, seg core.SegmentID, err error) {
	t.Helper()
	if e := r.seam.Schedule(journaltest.Failing(journaltest.OpWrite, format.SegmentName(seg), 1, err)); e != nil {
		t.Fatal(e)
	}
}

// appendFailing appends one more record of f and wants the append to fail.
func (r *rig) appendFailing(t *testing.T, f core.FileID, off int64) {
	t.Helper()
	h := format.RecordHeader{Kind: format.KindWrite, File: f, Extent: core.Extent{Off: off, Len: 1}, Version: core.Version{Counter: uint64(off + 1)}, Tag: 1}
	if _, err := r.streams.For(f).Append(h, format.RecordBody{Payload: []byte("x"), MTime: 1}); err == nil {
		t.Fatal("the append was expected to fail")
	}
}

func wantSyncError(t *testing.T, err error, seg core.SegmentID, files ...core.FileID) {
	t.Helper()
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync = %v, want a *core.SyncError", err)
	}
	if se.Segment != seg {
		t.Errorf("SyncError.Segment = %d, want %d", se.Segment, seg)
	}
	for _, f := range files {
		if !slices.Contains(se.Files, f) {
			t.Errorf("SyncError.Files = %v, missing %v", se.Files, f)
		}
	}
}

// TestSyncOverARecordInAFailedAppendsSegmentFails covers the issue's row: a
// file with a record in a segment a failed append ended gets a SyncError
// naming that segment, not a hang.
func TestSyncOverARecordInAFailedAppendsSegmentFails(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f, g := file(1), file(2)
	a := r.appendWrite(t, f, 0, "a")
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, g, 0)
	if b := r.appendWrite(t, g, 0, "b"); b.Segment == a.Segment {
		t.Fatal("the append after the failed one stayed in the ended segment")
	}
	err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f))
	wantSyncError(t, err, a.Segment, f)
	if r.d.Durable(a) {
		t.Error("the record of the failed segment reads durable")
	}
}

// TestFailedSyncIsNeverSatisfiedByALaterSync covers R14 and the RFC's row: two
// files' records in one segment whose sync failed stay failed, before and
// after a later sync report for the segment.
func TestFailedSyncIsNeverSatisfiedByALaterSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	fa, fb := file(1), file(2)
	a := r.appendWrite(t, fa, 0, "a")
	b := r.appendWrite(t, fb, 0, "b")
	if a.Segment != b.Segment {
		t.Fatal("the records did not share a segment")
	}
	r.failSyncOf(t, a.Segment, errors.New("sync boom"))

	wantSyncError(t, mustReturn(t, "Sync", r.syncAsync(context.Background(), fa, fb)), a.Segment, fa, fb)
	r.waitWindow(t, 0)
	wantSyncError(t, mustReturn(t, "Sync after the window", r.syncAsync(context.Background(), fa, fb)), a.Segment, fa, fb)

	// A later sync report for the failed segment must satisfy nothing.
	r.sinks[0].Synced(a.Segment, b.Off+b.Len)
	wantSyncError(t, mustReturn(t, "Sync after a later report", r.syncAsync(context.Background(), fa, fb)), a.Segment, fa, fb)
	if r.d.Durable(a) || r.d.Durable(b) {
		t.Errorf("a record of the failed window reads durable: a=%v b=%v", r.d.Durable(a), r.d.Durable(b))
	}
}

// TestSealFailureCountsAndFailsItsWindow covers R85 and the EndSeal part of
// the counters: a seal whose sync fails ends the segment, the window arrives
// by the seal's event, its waiters fail, and the failure is counted once, with
// its time.
func TestSealFailureCountsAndFailsItsWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f := file(1)
	sealBoom := errors.New("seal boom")
	a := r.appendWrite(t, f, 0, "a")
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, sealBoom)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal was expected to fail")
	}
	w := r.waitWindow(t, 0)
	if w.cause != segment.EndSeal || w.seg != a.Segment || !slices.Equal(w.locs, []segment.Location{a}) {
		t.Errorf("window = %+v, want the seal's window of %+v", w, a)
	}
	wantSyncError(t, mustReturn(t, "Sync", r.syncAsync(context.Background(), f)), a.Segment, f)

	eventually(t, "the seal failure to be counted", func() bool { return r.d.Stats().SyncFailures == 1 })
	got := r.d.Stats().LastSyncError
	if got == nil || !errors.Is(got.Err, sealBoom) {
		t.Fatalf("LastSyncError = %+v, want the seal's failure", got)
	}
	if want := r.clock.Now(); !got.At.Equal(want) {
		t.Errorf("LastSyncError.At = %v, want %v", got.At, want)
	}
}

// TestAppendFailureTakesItsWindowAndCountsNothing covers the EndAppend part
// of the counters: the window is taken, and no sync failed.
func TestAppendFailureTakesItsWindowAndCountsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, f, 1)
	w := r.waitWindow(t, 0)
	if w.cause != segment.EndAppend {
		t.Errorf("window cause = %v, want append", w.cause)
	}
	if got := r.d.Stats(); got.SyncFailures != 0 || got.LastSyncError != nil {
		t.Errorf("an append failure was counted as a sync failure: %+v", got)
	}
}

// TestWindowIsTheRecordsSinceTheLastSuccessfulSync covers R15: the records
// written since the last covering sync, and only those, leave the ledger for
// the window.
func TestWindowIsTheRecordsSinceTheLastSuccessfulSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f := file(1)
	synced := r.appendWrite(t, f, 0, "a")
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	b := r.appendWrite(t, f, 1, "b")
	c := r.appendWrite(t, f, 2, "c")
	r.failNextWrite(t, b.Segment, errors.New("write boom"))
	r.appendFailing(t, f, 3)

	w := r.waitWindow(t, 0)
	if !slices.Equal(w.locs, []segment.Location{b, c}) {
		t.Errorf("window = %v, want the records since the last sync %v, %v (not %v)", w.locs, b, c, synced)
	}
	eventually(t, "the ledger to hold nothing of the window", func() bool { return r.ledgerLen(0) == 0 })
}

// TestARecordStillWritingWhenTheSyncFailsIsInTheWindow covers E5's hand-over:
// the end event waits for the slot still writing, and that record is in the
// window, so its file's Sync fails instead of hanging.
func TestARecordStillWritingWhenTheSyncFailsIsInTheWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f, g, h := file(1), file(2), file(3)
	first := r.appendWrite(t, h, 0, "h")
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), h)); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	r.settlePool(t)

	hold := r.seam.HoldFirst(t, journaltest.OpWrite, format.SegmentName(first.Segment))
	var held segment.Location
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		hd := format.RecordHeader{Kind: format.KindWrite, File: f, Extent: core.Extent{Len: 1}, Version: core.Version{Counter: 1}, Tag: 1}
		p, err := r.streams.For(f).Append(hd, format.RecordBody{Payload: []byte("f"), MTime: 1})
		if err != nil {
			t.Errorf("the held append: %v", err)
		}
		held = p.Loc
	}()
	<-hold.Reached()
	other := r.appendWrite(t, g, 0, "g")
	r.failSyncOf(t, first.Segment, errors.New("sync boom"))
	wantSyncError(t, mustReturn(t, "Sync of the later record", r.syncAsync(context.Background(), g)), first.Segment, g)

	hold.Release()
	wg.Wait()
	w := r.waitWindow(t, 0)
	for _, loc := range []segment.Location{held, other} {
		if !slices.Contains(w.locs, loc) {
			t.Errorf("window %v misses %v", w.locs, loc)
		}
	}
	wantSyncError(t, mustReturn(t, "Sync of the held record", r.syncAsync(context.Background(), f)), first.Segment, f)
}

// recorder collects what the layer announces.
type recorder[T any] struct {
	mu  sync.Mutex
	got []T
}

func (c *recorder[T]) add(v T) {
	c.mu.Lock()
	c.got = append(c.got, v)
	c.mu.Unlock()
}

func (c *recorder[T]) all() []T {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got)
}

type resolvedAt struct {
	stream uint32
	seg    core.SegmentID
}

// TestAnEmptyWindowIsAnnouncedResolvedAtOnce covers R19 for a window that
// holds nothing: a failed append over a segment a sync already covered has no
// record to fail, so the segment is announced at once.
func TestAnEmptyWindowIsAnnouncedResolvedAtOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var got recorder[resolvedAt]
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(stream uint32, seg core.SegmentID) { got.add(resolvedAt{stream, seg}) }
	})
	r.d.keepWindows.Store(true)
	r.d.Start(r.streams)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, f, 1)

	eventually(t, "the empty window to be announced", func() bool { return len(got.all()) == 1 })
	if want := (resolvedAt{0, a.Segment}); got.all()[0] != want {
		t.Errorf("announced %+v, want %+v", got.all()[0], want)
	}
	if w := r.windowsOf(0); len(w) != 0 {
		t.Errorf("an empty window is still pending: %+v", w)
	}
}

// TestAResolvedWindowIsAnnouncedOnceAndItsRecordsAreNotPending covers R19 and
// the clean-up of resolve: the segment is announced once, the window and the
// failure kept for its segment are gone, and its records are not pending.
func TestAResolvedWindowIsAnnouncedOnceAndItsRecordsAreNotPending(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var got recorder[resolvedAt]
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(stream uint32, seg core.SegmentID) { got.add(resolvedAt{stream, seg}) }
	})
	r.d.keepWindows.Store(true)
	r.d.Start(r.streams)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	r.failNextWrite(t, a.Segment, errors.New("write boom"))
	r.appendFailing(t, f, 1)
	r.waitWindow(t, 0)
	if n := len(got.all()); n != 0 {
		t.Fatalf("a window holding a record was announced before it was resolved: %d", n)
	}

	st := r.d.made[0]
	st.mu.Lock()
	w := st.windows[0]
	if !st.pending(a) {
		t.Error("the record of a pending window is not pending")
	}
	st.mu.Unlock()
	r.d.resolve(st, w)

	if g := got.all(); len(g) != 1 || g[0] != (resolvedAt{0, a.Segment}) {
		t.Fatalf("announced %+v, want the segment once", g)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pending(a) {
		t.Error("the record of a resolved window is still pending")
	}
	if _, ok := st.failed[a.Segment]; ok {
		t.Error("the resolved segment's failure was kept")
	}
	if len(st.windows) != 0 {
		t.Errorf("windows = %d, want the resolved one dropped", len(st.windows))
	}
}

// leaveAFailedSegment drives the segment a stream took to leave a failed one: it
// fails a sync of f, then appends g's record in the spare. The two files are
// distinct so a Sync of g does not meet the failed segment's window.
func (r *rig) leaveAFailedSegment(t *testing.T, f, g core.FileID) (first, spare segment.Location) {
	t.Helper()
	first = r.appendWrite(t, f, 0, "a")
	r.failSyncOf(t, first.Segment, errors.New("first boom"))
	wantSyncError(t, mustReturn(t, "Sync", r.syncAsync(context.Background(), f)), first.Segment, f)
	r.waitWindow(t, 0)
	r.settlePool(t)
	spare = r.appendWrite(t, g, 1, "b")
	if spare.Segment == first.Segment {
		t.Fatal("the append after the failed sync did not leave the failed segment")
	}
	return first, spare
}

// TestAFailureInTheSpareBeforeItsFirstSyncStopsTheStream covers R24 and R28:
// the second refusal reaches the read-only entry once, with the second
// failure, and the stream takes no third spare.
func TestAFailureInTheSpareBeforeItsFirstSyncStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var ro recorder[error]
	r := newRigConfigured(t, 1, func(*Config) {})
	r.d.keepWindows.Store(true)
	r.d.readOnly = ro.add
	r.d.Start(r.streams)
	f, g := file(1), file(2)
	_, spare := r.leaveAFailedSegment(t, f, g)
	// taken counts the segments the streams have taken out of the pool: the
	// set's segments, which a spare joins only when a stream takes it, so the
	// pool's refills do not move it.
	taken := func() int64 { return r.set.Counters().Segments }
	if got := taken(); got != 2 {
		t.Fatalf("%d segments taken after the first failure, want the failed one and the spare", got)
	}
	second := errors.New("second boom")
	r.failSyncOf(t, spare.Segment, second)
	r.settlePool(t)
	wantSyncError(t, mustReturn(t, "Sync in the spare", r.syncAsync(context.Background(), g)), spare.Segment, g)

	eventually(t, "the read-only entry to be called", func() bool { return len(ro.all()) == 1 })
	if got := ro.all()[0]; !errors.Is(got, second) {
		t.Errorf("the read-only entry got %v, want the second failure", got)
	}
	h := format.RecordHeader{Kind: format.KindWrite, File: g, Extent: core.Extent{Off: 2, Len: 1}, Version: core.Version{Counter: 3}, Tag: 1}
	_, err := r.streams.For(g).Append(h, format.RecordBody{Payload: []byte("c"), MTime: 1})
	if !errors.Is(err, segment.ErrStreamStopped) || !errors.Is(err, second) {
		t.Errorf("the append after the second refusal = %v, want ErrStreamStopped wrapping the failure", err)
	}
	if n := len(ro.all()); n != 1 {
		t.Errorf("the read-only entry was called %d times, want once", n)
	}
	r.settlePool(t)
	if got := taken(); got != 2 {
		t.Errorf("%d segments taken after the second refusal, want 2: the stream rolled into a third", got)
	}
}

// TestAFailureAfterTheSparesFirstSyncRollsOn covers the variant: a spare whose
// first sync succeeded and a later one failed is an ordinary failed segment.
func TestAFailureAfterTheSparesFirstSyncRollsOn(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var ro recorder[error]
	r := newRigConfigured(t, 1, func(*Config) {})
	r.d.keepWindows.Store(true)
	r.d.readOnly = ro.add
	r.d.Start(r.streams)
	f, g := file(1), file(2)
	_, spare := r.leaveAFailedSegment(t, f, g)
	if err := mustReturn(t, "Sync in the spare", r.syncAsync(context.Background(), g)); err != nil {
		t.Fatalf("the spare's first Sync = %v", err)
	}
	r.appendWrite(t, g, 2, "c")
	r.failSyncOf(t, spare.Segment, errors.New("later boom"))
	wantSyncError(t, mustReturn(t, "later Sync", r.syncAsync(context.Background(), g)), spare.Segment, g)
	r.waitWindowOf(t, 0, spare.Segment)
	r.settlePool(t)
	if next := r.appendWrite(t, g, 3, "d"); next.Segment == spare.Segment {
		t.Error("the stream did not roll out of the failed spare")
	}
	if n := len(ro.all()); n != 0 {
		t.Errorf("the read-only entry was called %d times for a failure after a sync", n)
	}
}

// waitWindowOf waits for stream no to hold a pending window of seg.
func (r *rig) waitWindowOf(t *testing.T, no int, seg core.SegmentID) windowView {
	t.Helper()
	var got windowView
	eventually(t, "the worker to take the segment's window", func() bool {
		for _, w := range r.windowsOf(no) {
			if w.seg == seg {
				got = w
				return true
			}
		}
		return false
	})
	return got
}
