package durability

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// failByTimer fails the next sync of seg and lets the sync bound issue it, so
// that no Sync call waits for it.
func (fr *failRig) failByTimer(t *testing.T, seg core.SegmentID) {
	t.Helper()
	fr.failSyncOf(t, seg, errors.New("sync boom"))
	// The bound runs from when the worker first sees the record, and the worker
	// arms its timer after it has stamped; the clock moves until the sync that
	// the schedule fails has been issued.
	fr.stamped(t, 0)
	before := fr.d.Stats().SyncFailures
	eventually(t, "the sync bound to issue the failing sync", func() bool {
		fr.clock.Advance(2 * core.SyncBound)
		return fr.d.Stats().SyncFailures > before
	})
}

// wantNilSync fails unless a Sync of the files returns nil.
func (fr *failRig) wantNilSync(t *testing.T, ids ...core.FileID) {
	t.Helper()
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), ids...)); err != nil {
		t.Fatalf("Sync(%v) = %v, want nil", ids, err)
	}
}

// TestAFailedWindowIsReportedOnceToEachFileThatLostARecord covers a failure
// no Sync was waiting for: the next Sync naming the file gets the error, once,
// and a file with no record in the window never does.
func TestAFailedWindowIsReportedOnceToEachFileThatLostARecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f, g := file(1), file(2)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	fr.wantNilSync(t, g)

	fr.failByTimer(t, loc.Segment)
	fr.awaitResolved(t)

	fr.wantNilSync(t, g)
	wantSyncError(t, mustReturn(t, "Sync of the failed file", fr.syncAsync(context.Background(), f)), loc.Segment, f)
	fr.wantNilSync(t, f)
	fr.wantNilSync(t, g)
}

// TestASyncAnswersAFailedRecordOnlyAfterTheLossIsRecorded pins that the answer
// follows the generation's raise and the loss event.
func TestASyncAnswersAFailedRecordOnlyAfterTheLossIsRecorded(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	before := fr.d.Generation()

	fr.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f))
	wantSyncError(t, err, loc.Segment, f)
	if got := fr.d.Generation(); got != before+1 {
		t.Errorf("the generation is %d when Sync answered, want %d", got, before+1)
	}
	evs := fr.events()
	if len(evs) != 1 || evs[0].File != f {
		t.Errorf("loss events %+v when Sync answered, want one naming the file", evs)
	}
}

// TestAFailedWindowDoesNotWedgeTheFile covers a file whose failed record is
// gone: the report comes once, and a later Sync waits only for later records.
func TestAFailedWindowDoesNotWedgeTheFile(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a, _ := fr.publish(t, f, 0, "aaaa")
	fr.failByTimer(t, a.Segment)
	fr.awaitResolved(t)
	b, _ := fr.publish(t, f, 8, "bbbb")
	if b.Segment == a.Segment {
		t.Fatal("the write after the failure stayed in the failed segment")
	}

	wantSyncError(t, mustReturn(t, "first Sync", fr.syncAsync(context.Background(), f)), a.Segment, f)
	fr.wantNilSync(t, f)
	if p, ok := fr.ix.Lookup(f, 0); ok {
		t.Errorf("the failed write is still held: %+v", p)
	}
	if p, ok := fr.ix.Lookup(f, 8); !ok {
		t.Errorf("the later write is not held: %+v", p)
	}
	if evs := fr.events(); len(evs) != 1 || evs[0].File != f {
		t.Errorf("loss events = %+v, want one naming the file", evs)
	}
}

// TestAMarkIsNotSetAgainForAWindowAlreadyReported covers a window that fails a
// further record of a file it already reported while it is still pending: the
// file is told once.
func TestAMarkIsNotSetAgainForAWindowAlreadyReported(t *testing.T) {
	prof.Watch(t, prof.Options{})
	atFail := make(chan struct{})
	release, open := releaser(t)
	fr := newFailRig(t, func(fr *failRig) {
		t.Cleanup(open)
		fr.d.afterFail = func() {
			close(atFail)
			<-release
		}
	})
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	fr.failByTimer(t, loc.Segment)
	<-atFail

	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)), loc.Segment, f)
	st := fr.d.made[0]
	st.mu.Lock()
	w := st.windows[0]
	st.markLocked(w, f)
	st.mu.Unlock()
	open()
	fr.awaitResolved(t)

	fr.wantNilSync(t, f)
}

// TestACancelledSyncClearsNoMark covers a caller that is gone: a done context
// returns before any mark is read, and a waiter cancelled while the sync fails
// leaves the mark for the next call.
func TestACancelledSyncClearsNoMark(t *testing.T) {
	prof.Watch(t, prof.Options{})
	atSync := make(chan struct{}, 1)
	release, open := releaser(t)
	fr := newFailRig(t, func(fr *failRig) {
		t.Cleanup(open)
		fr.d.afterSync = func() {
			select {
			case atSync <- struct{}{}:
				<-release
			default:
			}
		}
	})
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	fr.failSyncOf(t, loc.Segment, errors.New("sync boom"))

	ctx, cancel := context.WithCancel(context.Background())
	ch := fr.syncAsync(ctx, f)
	<-atSync
	mustBlock(t, "Sync while the sync is held", ch)
	cancel()
	if err := mustReturn(t, "cancelled Sync", ch); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Sync = %v, want context.Canceled", err)
	}
	open()
	fr.awaitResolved(t)

	done, stop := context.WithCancel(context.Background())
	stop()
	if err := mustReturn(t, "Sync with a done context", fr.syncAsync(done, f)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync with a done context = %v, want context.Canceled", err)
	}
	wantSyncError(t, mustReturn(t, "live Sync", fr.syncAsync(context.Background(), f)), loc.Segment, f)
	fr.wantNilSync(t, f)
}

// TestASyncNamesEveryFileOfTheCallThatFailed covers one error for a call that
// names several failed files, after which none of them is reported again.
func TestASyncNamesEveryFileOfTheCallThatFailed(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f, g, h := file(1), file(2), file(3)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	fr.publish(t, g, 0, "gggg")
	fr.failByTimer(t, loc.Segment)
	fr.awaitResolved(t)

	err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f, g, h))
	wantSyncError(t, err, loc.Segment, f, g)
	var se *core.SyncError
	if errors.As(err, &se) && slices.Contains(se.Files, h) {
		t.Errorf("SyncError.Files = %v names a file that lost nothing", se.Files)
	}
	fr.wantNilSync(t, f)
	fr.wantNilSync(t, g)
}

// TestTwoFailedWindowsOfOneFileAreReportedByOneError covers a file that lost
// records in two windows before any Sync: the older window's segment is named
// and both are cleared.
func TestTwoFailedWindowsOfOneFileAreReportedByOneError(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a, _ := fr.publish(t, f, 0, "aaaa")
	fr.failByTimer(t, a.Segment)
	fr.awaitResolved(t)
	b, _ := fr.publish(t, f, 8, "bbbb")
	fr.failByTimer(t, b.Segment)
	fr.awaitResolved(t)
	if a.Segment == b.Segment {
		t.Fatal("both writes landed in one segment")
	}

	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)), a.Segment, f)
	fr.wantNilSync(t, f)
}

// TestASyncOfTwoFilesNamesTheOldestWindowAcrossThem covers the choice of the
// window a SyncError names when the files of one call lost records in
// different windows: the older one's segment, whichever file is listed first.
func TestASyncOfTwoFilesNamesTheOldestWindowAcrossThem(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f, g := file(1), file(2)
	a, _ := fr.publish(t, g, 0, "gggg")
	fr.failByTimer(t, a.Segment)
	fr.awaitResolved(t)
	b, _ := fr.publish(t, f, 0, "ffff")
	fr.failByTimer(t, b.Segment)
	fr.awaitResolved(t)
	if a.Segment == b.Segment {
		t.Fatal("both writes landed in one segment")
	}

	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f, g)), a.Segment, f, g)
}

// TestWaitDurableNeverConsultsTheMarks pins that the durability wait neither
// reports a failed window nor takes the report a Sync is owed.
func TestWaitDurableNeverConsultsTheMarks(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	fr.failByTimer(t, loc.Segment)
	fr.awaitResolved(t)

	got := make(chan error, 1)
	go func() { got <- fr.d.WaitDurable(loc) }()
	select {
	case err := <-got:
		t.Fatalf("WaitDurable of a failed record returned %v, want it to keep waiting", err)
	case <-time.After(50 * time.Millisecond):
	}
	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)), loc.Segment, f)
}
