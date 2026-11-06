package durability

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestASyncNamingFilesOnTwoStreamsReportsBothFailures covers R17 and R44 across
// streams: one Sync names every file the failures of both streams took, and
// clears the mark of each, so no later Sync reports an old failure again.
func TestASyncNamingFilesOnTwoStreamsReportsBothFailures(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigConfigured(t, 2, func(*Config) {})
	r.d.Start(r.streams)
	f, g := r.fileOn(t, 0), r.fileOn(t, 1)
	lf, _ := r.publish(t, f, 0, "aaaa")
	lg, _ := r.publish(t, g, 0, "gggg")
	if err := r.seam.Schedule(
		journaltest.Failing(journaltest.OpSync, format.SegmentName(lf.Segment), 1, errors.New("boom f")),
		journaltest.Failing(journaltest.OpSync, format.SegmentName(lg.Segment), 1, errors.New("boom g")),
	); err != nil {
		t.Fatal(err)
	}
	r.stamped(t, 0)
	r.stamped(t, 1)
	eventually(t, "both syncs to fail", func() bool {
		r.clock.Advance(2 * core.SyncBound)
		return r.d.Stats().SyncFailures >= 2
	})
	r.waitResolved(t, 0)
	r.waitResolved(t, 1)

	err := mustReturn(t, "Sync of both", r.syncAsync(context.Background(), f, g))
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync = %v, want a *core.SyncError", err)
	}
	if !slices.Contains(se.Files, f) || !slices.Contains(se.Files, g) {
		t.Errorf("SyncError.Files = %v, want both files", se.Files)
	}
	for _, id := range []core.FileID{f, g} {
		if err := mustReturn(t, "Sync after the report", r.syncAsync(context.Background(), id)); err != nil {
			t.Errorf("Sync(%v) after the report = %v, want nil: its mark was left set", id, err)
		}
	}
}

// TestTwoWaitersOnAFailedRecordReturnAndOneIsTold pins that a Sync whose target
// was failed, and whose file's mark another Sync took, returns instead of
// waiting for a record that is gone.
func TestTwoWaitersOnAFailedRecordReturnAndOneIsTold(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	gate, open := releaser(t)
	r.d.beforeFail = func(*window) { <-gate }
	t.Cleanup(open)
	r.d.Start(r.streams)
	f := file(1)
	loc, _ := r.publish(t, f, 0, "aaaa")
	r.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	first, second := r.syncAsync(context.Background(), f), r.syncAsync(context.Background(), f)
	// Both waiters hold the failed record as their target before the window
	// is failed.
	eventually(t, "the window to be taken", func() bool { return len(r.windowsOf(0)) > 0 })
	open()

	told := 0
	for _, ch := range []<-chan error{first, second} {
		err := mustReturn(t, "a waiter", ch)
		if err == nil {
			continue
		}
		wantSyncError(t, err, loc.Segment, f)
		told++
	}
	if told != 1 {
		t.Errorf("%d waiters were told of the failure, want exactly one", told)
	}
}

// TestALossRecordWhoseSyncFailsEndsItsSegment covers the stream not wedging
// when the sync of a loss record fails: the next record is refused or its
// Sync is answered, and never left waiting.
func TestALossRecordWhoseSyncFailsEndsItsSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f, g := file(1), file(2)
	a, _ := fr.publish(t, f, 0, "aaaa")
	if err := fr.seam.Schedule(
		journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, errors.New("boom 1")),
		journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment+1), 1, errors.New("boom 2")),
	); err != nil {
		t.Fatal(err)
	}
	_ = mustReturn(t, "Sync of f", fr.syncAsync(context.Background(), f))

	// The loss record went to the spare, whose sync failed. A write after it
	// either is refused by the stopped stream or gets its Sync answered.
	v, err := fr.d.counters.Draw()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fr.streams.For(g).Append(format.RecordHeader{
		Kind: format.KindWrite, File: g, Extent: core.Extent{Len: 4}, Version: v, Tag: 1,
	}, format.RecordBody{Payload: []byte("gggg"), MTime: 1})
	if err != nil {
		if !errors.Is(err, segment.ErrStreamStopped) {
			t.Fatalf("Append after the loss record = %v", err)
		}
		return
	}
	_ = mustReturn(t, "Sync of g", fr.syncAsync(context.Background(), g))
}

// TestACoverStillInALaterWindowIsNotLiftedWhenAnEarlierOneFails pins that the
// cover of a failed record, a newer record that took over what it had
// superseded, is lifted only when no window or ledger entry of the stream
// still holds it: here the cover's own window fails next, and the extent the
// first failure restored has to be held again for a reader (R26).
func TestACoverStillInALaterWindowIsNotLiftedWhenAnEarlierOneFails(t *testing.T) {
	prof.Watch(t, prof.Options{})
	// No worker runs, so the test takes the two ends and fails the windows
	// itself, with both windows pending together.
	r := newRigUnstarted(t, 1)
	f := file(1)
	st := r.d.made[0]
	r.streams.OnEnd(r.d.onEnd)

	_, v1 := r.publish(t, f, 0, "aaaa")
	r.syncStream(t, f)
	a, _ := r.publish(t, f, 0, "bbbb")
	r.failNextWrite(t, a.Segment, errors.New("write boom a"))
	r.appendFailing(t, f, 100)
	r.settlePool(t)
	b, _ := r.publish(t, f, 0, "cccc")
	if b.Segment == a.Segment {
		t.Fatal("the append after the failed write stayed in the failed segment")
	}
	r.failNextWrite(t, b.Segment, errors.New("write boom b"))
	r.appendFailing(t, f, 100)

	eventually(t, "both ends to be queued", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.ends) == 2
	})
	r.d.takeWindows(st)
	if n := len(r.windowsOf(0)); n != 2 {
		t.Fatalf("%d windows pending, want both", n)
	}
	r.d.failWindows(st)

	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v1 {
		t.Fatalf("after both windows failed Lookup serves %+v, %v, want v1's piece held again", p, ok)
	}
}

// TestASyncOfARecordASyncAlreadyCoveredWaitsForTheLift pins that a Sync issued
// after a sync covered the record, but before the worker lifted what the
// record superseded, does not return until the lift: a Sync that returned has
// seen the superseded extent released.
func TestASyncOfARecordASyncAlreadyCoveredWaitsForTheLift(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	r.publish(t, f, 0, "bbbb")

	// The test holds the file's domain, so the worker, which lifts under it,
	// stops there once a sync has covered v2.
	r.domains.mu.Lock()
	before := r.domains.taken
	r.domains.mu.Unlock()
	unlock := r.domains.Lock(f)
	released := false
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	t.Cleanup(release)
	r.syncStream(t, f)
	eventually(t, "the worker to ask for the file's domain", func() bool {
		r.domains.mu.Lock()
		defer r.domains.mu.Unlock()
		return r.domains.taken > before+1
	})

	done := r.syncAsync(context.Background(), f)
	mustBlock(t, "Sync of a covered record whose lift is pending", done)
	release()
	if err := mustReturn(t, "Sync", done); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("after the Sync %+v is still retained", got)
	}
}
