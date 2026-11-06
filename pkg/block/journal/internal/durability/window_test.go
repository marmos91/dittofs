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
// after a later sync attempt on the segment answers.
func TestFailedSyncIsNeverSatisfiedByALaterSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
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

	// A later sync of the failed segment answers, and the seam would let it
	// succeed: it must satisfy nothing.
	_ = r.streams.For(fa).Sync()
	wantSyncError(t, mustReturn(t, "Sync after a later attempt", r.syncAsync(context.Background(), fa, fb)), a.Segment, fa, fb)
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
