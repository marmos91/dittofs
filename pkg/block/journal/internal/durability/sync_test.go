package durability

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// syncsOf counts the syncs the seam saw of the segment's file.
func (r *rig) syncsOf(seg core.SegmentID) int {
	return r.seam.CallsFor(journaltest.OpSync, format.SegmentName(seg))
}

// syncAsync runs Sync in a goroutine and returns the channel its error lands on.
func (r *rig) syncAsync(ctx context.Context, ids ...core.FileID) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- r.d.Sync(ctx, ids...) }()
	return ch
}

func mustReturn(t *testing.T, what string, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

func mustBlock(t *testing.T, what string, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s returned (%v) while its sync was held", what, err)
	case <-time.After(30 * time.Millisecond):
	}
}

// TestGroupCommitSharesSyncsAcrossWaiters covers: 16 writers each appending
// and syncing 8 times, with every sync parked until all active writers wait,
// cost far fewer than one sync per Sync call. A sync per waiter would issue
// 128.
func TestGroupCommitSharesSyncsAcrossWaiters(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	first := r.appendWrite(t, file(100), 0, "seed")
	if err := r.d.Sync(context.Background(), file(100)); err != nil {
		t.Fatalf("seed Sync: %v", err)
	}
	seg := format.SegmentName(first.Segment)
	gate := &journaltest.SimGate{Match: func(op string, key []byte) bool {
		return op == journaltest.OpSync && string(key) == seg
	}}
	r.seam.SetFault(gate)
	gate.Arm()
	t.Cleanup(gate.Disarm)
	base := r.syncsOf(first.Segment)

	const writers, rounds = 16, 8
	var active, waiting atomic.Int64
	active.Store(writers)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer active.Add(-1)
			f := file(byte(w + 1))
			for i := range rounds {
				r.appendWrite(t, f, int64(i*8), "x")
				waiting.Add(1)
				if err := r.d.Sync(context.Background(), f); err != nil {
					t.Errorf("Sync: %v", err)
				}
				waiting.Add(-1)
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	for done := false; !done; {
		select {
		case <-finished:
			done = true
		default:
			if gate.Parked() > 0 && waiting.Load() >= active.Load() {
				gate.Release()
			}
			time.Sleep(50 * time.Microsecond)
		}
	}
	if got := r.syncsOf(first.Segment) - base; got > writers*rounds/4 {
		t.Fatalf("%d syncs for %d Sync calls, want at most %d: waiters are not sharing a sync", got, writers*rounds, writers*rounds/4)
	}
}

// TestSyncReturnsAfterTheCoveringSync covers: a Sync blocks while the sync
// that covers its record is held, and a Sync naming nothing unsynced, or an
// unknown file, issues no sync.
func TestSyncReturnsAfterTheCoveringSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))

	ch := r.syncAsync(context.Background(), f)
	<-h.Reached()
	mustBlock(t, "Sync", ch)
	h.Release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !r.d.Durable(a) {
		t.Error("the record is not durable after Sync returned")
	}

	before := r.syncsOf(a.Segment)
	if err := r.d.Sync(context.Background(), f, file(99)); err != nil {
		t.Fatalf("Sync of nothing unsynced: %v", err)
	}
	if err := r.d.Sync(context.Background()); err != nil {
		t.Fatalf("Sync naming no file: %v", err)
	}
	if got := r.syncsOf(a.Segment); got != before {
		t.Errorf("a Sync with nothing to cover issued %d syncs", got-before)
	}
}

// TestRecordCompletedAfterTheSyncBeganIsNotCovered covers: a record whose
// slot completed after the running sync began is not told it is durable by
// that sync; its Sync issues a second one.
func TestRecordCompletedAfterTheSyncBeganIsNotCovered(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))

	chA := r.syncAsync(context.Background(), f)
	<-h.Reached()
	b := r.appendWrite(t, f, 8, "b")
	chB := r.syncAsync(context.Background(), f)
	mustBlock(t, "Sync of b", chB)
	h.Release()
	if err := mustReturn(t, "Sync of a", chA); err != nil {
		t.Fatalf("Sync of a: %v", err)
	}
	// chA and chB both name f, so chB's target moved with b; b was not
	// covered by the first sync and needs a second.
	if err := mustReturn(t, "Sync of b", chB); err != nil {
		t.Fatalf("Sync of b: %v", err)
	}
	if !r.d.Durable(b) {
		t.Error("b is not durable after its Sync returned")
	}
	if got := r.syncsOf(a.Segment); got != 2 {
		t.Errorf("%d syncs, want 2: the first cannot cover a record appended after it began", got)
	}
}

// TestWaitDurableNeedsNoSyncCaller covers: WaitDurable returns once the
// record is synced by anyone, and Stop wakes a parked waiter with ErrClosed.
func TestWaitDurableNeedsNoSyncCaller(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	ha := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))

	ch := make(chan error, 1)
	go func() { ch <- r.d.WaitDurable(a) }()
	<-ha.Reached()
	mustBlock(t, "WaitDurable", ch)
	ha.Release()
	if err := mustReturn(t, "WaitDurable", ch); err != nil {
		t.Fatalf("WaitDurable: %v", err)
	}

	b := r.appendWrite(t, f, 8, "b")
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(b.Segment))
	parked := make(chan error, 1)
	go func() { parked <- r.d.WaitDurable(b) }()
	<-h.Reached() // WaitDurable led a sync, and the seam holds it
	stopped := make(chan struct{})
	go func() { r.d.Stop(); close(stopped) }()
	if err := mustReturn(t, "parked WaitDurable", parked); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("parked WaitDurable after Stop = %v, want ErrClosed", err)
	}
	h.Release()
	<-stopped
	if err := r.d.WaitDurable(b); err != nil && !errors.Is(err, core.ErrClosed) {
		t.Fatalf("WaitDurable after Stop = %v", err)
	}
}

// TestSyncRidesTheSealsSync covers: a Sync waiting on a record of a segment
// that a seal is closing returns once the seal's sync reports, not before.
func TestSyncRidesTheSealsSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))

	sealed := make(chan error, 1)
	go func() { _, err := r.streams.For(f).Seal(false); sealed <- err }()
	<-h.Reached()
	ch := r.syncAsync(context.Background(), f)
	mustBlock(t, "Sync", ch)
	h.Release()
	if err := mustReturn(t, "Seal", sealed); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !r.d.Durable(a) {
		t.Error("the record is not durable after the seal's sync")
	}
}

// TestSyncNeverSyncsADirectory covers: no directory sync, and every sync
// names a segment file.
func TestSyncNeverSyncsADirectory(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	r.appendWrite(t, f, 0, "a")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	r.settlePool(t)
	dirs, all := r.seam.Calls(journaltest.OpSyncDir), r.seam.Calls(journaltest.OpSync)
	seg := r.d.made[0].ledgerSegForTest()
	for i := range 5 {
		r.appendWrite(t, f, int64(8*(i+1)), "b")
		if err := r.d.Sync(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.seam.Calls(journaltest.OpSyncDir); got != dirs {
		t.Errorf("%d directory syncs during writes and Syncs", got-dirs)
	}
	if got, want := r.seam.Calls(journaltest.OpSync)-all, r.syncsOf(seg)-1; got < want {
		t.Errorf("%d syncs of any file against %d of the segment file", got, want)
	}
}

// ledgerSegForTest returns the segment of the last record tracked.
func (st *stream) ledgerSegForTest() core.SegmentID {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastSeg
}

// TestCancelledSyncLeavesTheSyncRunning covers: a cancelled caller returns
// the context's error at once while the sync runs on for the other waiters,
// and a context already done issues no sync.
func TestCancelledSyncLeavesTheSyncRunning(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))

	ctx, cancel := context.WithCancel(context.Background())
	leader := r.syncAsync(ctx, f)
	<-h.Reached()
	other := r.syncAsync(context.Background(), f)
	cancel()
	if err := mustReturn(t, "cancelled Sync", leader); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Sync = %v, want context.Canceled", err)
	}
	mustBlock(t, "the other Sync", other)
	h.Release()
	if err := mustReturn(t, "the other Sync", other); err != nil {
		t.Fatalf("the other Sync = %v", err)
	}
	if got := r.syncsOf(a.Segment); got != 1 {
		t.Errorf("%d syncs, want 1", got)
	}

	r.appendWrite(t, f, 8, "b")
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if err := r.d.Sync(done, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync with a done context = %v", err)
	}
	if got := r.syncsOf(a.Segment); got != 1 {
		t.Errorf("a done context issued a sync: %d syncs", got)
	}
}

// TestFailedSyncFailsEveryWaiterAndIsNotRetried covers: every waiter on the
// failed sync gets a SyncError naming the segment, the failure is counted,
// and a later Sync of the segment fails at once with no new sync.
func TestFailedSyncFailsEveryWaiterAndIsNotRetried(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f, g := file(1), file(2)
	a := r.appendWrite(t, f, 0, "a")
	r.appendWrite(t, g, 0, "g")
	boom := errors.New("disk on fire")
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, boom)); err != nil {
		t.Fatal(err)
	}
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))

	chF := r.syncAsync(context.Background(), f)
	<-h.Reached()
	chG := r.syncAsync(context.Background(), g)
	h.Release()
	for name, ch := range map[string]<-chan error{"f": chF, "g": chG} {
		err := mustReturn(t, "Sync of "+name, ch)
		var se *core.SyncError
		if !errors.Is(err, core.ErrSyncFailed) || !errors.As(err, &se) || se.Segment != a.Segment {
			t.Fatalf("Sync of %s = %v, want a SyncError of segment %d", name, err, a.Segment)
		}
		if !errors.Is(err, boom) {
			t.Errorf("Sync of %s does not carry the cause: %v", name, err)
		}
		if want := map[string]core.FileID{"f": f, "g": g}[name]; len(se.Files) != 1 || se.Files[0] != want {
			t.Errorf("Sync of %s: SyncError.Files = %v, want [%v]", name, se.Files, want)
		}
	}
	if got := r.d.Stats().SyncFailures; got != 1 {
		t.Errorf("SyncFailures = %d, want 1", got)
	}

	before := r.syncsOf(a.Segment)
	err := r.d.Sync(context.Background(), f)
	var later *core.SyncError
	if !errors.Is(err, core.ErrSyncFailed) || !errors.As(err, &later) {
		t.Fatalf("a later Sync of the failed segment = %v, want ErrSyncFailed", err)
	}
	if len(later.Files) != 1 || later.Files[0] != f {
		t.Errorf("the later SyncError.Files = %v, want [%v]", later.Files, f)
	}
	if got := r.syncsOf(a.Segment); got != before {
		t.Errorf("a Sync retried the failed segment: %d new syncs", got-before)
	}
}

// TestWaitDurableOnAFailedSegmentKeepsWaiting pins that WaitDurable never
// returns a SyncError and does not retry a failed segment.
func TestWaitDurableOnAFailedSegmentKeepsWaiting(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	r.d.keepWindows.Store(true)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, errors.New("boom"))); err != nil {
		t.Fatal(err)
	}
	ch := make(chan error, 1)
	go func() { ch <- r.d.WaitDurable(a) }()
	eventually(t, "the failed sync", func() bool { return r.d.Stats().SyncFailures == 1 })
	mustBlock(t, "WaitDurable on a failed segment", ch)
	r.d.Stop()
	if err := mustReturn(t, "WaitDurable", ch); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("WaitDurable after Stop = %v, want ErrClosed", err)
	}
	if got := r.syncsOf(a.Segment); got != 1 {
		t.Errorf("WaitDurable retried a failed segment: %d syncs", got)
	}
}

// TestSyncAfterStopIsClosed pins that a stopped layer answers ErrClosed.
func TestSyncAfterStopIsClosed(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	r.appendWrite(t, f, 0, "a")
	r.d.Stop()
	if err := r.d.Sync(context.Background(), f); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Sync after Stop = %v, want ErrClosed", err)
	}
}

// TestStatsCountEachSync pins the counters and the histogram.
func TestStatsCountEachSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	for i := range 3 {
		r.appendWrite(t, f, int64(i*8), "x")
		if err := r.d.Sync(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	// A sync is counted just after the waiters it covered are released.
	eventually(t, "the third sync to be counted", func() bool { return r.d.Stats().Syncs == 3 })
	p := r.d.Stats()
	if p.Syncs != 3 || p.SyncFailures != 0 {
		t.Errorf("Stats = %+v, want 3 syncs, 0 failures", p)
	}
	var n uint64
	for _, c := range p.SyncDuration.Counts {
		n += c
	}
	if n != 3 {
		t.Errorf("the histogram counts %d syncs, want 3", n)
	}
}

type recordedSpan struct {
	op  core.SpanOp
	err error
	end bool
}

// TestEachIssuedSyncHasOneSpanEndedWithItsError pins the trace: one sync span
// per sync issued, ended with the sync's own error.
func TestEachIssuedSyncHasOneSpanEndedWithItsError(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var mu sync.Mutex
	var spans []*recordedSpan
	tracer := func(ctx context.Context, op core.SpanOp, _ ...slog.Attr) (context.Context, func(error)) {
		s := &recordedSpan{op: op}
		mu.Lock()
		spans = append(spans, s)
		mu.Unlock()
		return ctx, func(err error) {
			mu.Lock()
			s.err, s.end = err, true
			mu.Unlock()
		}
	}
	r := newRigStarted(t, 1, tracer)
	r.d.keepWindows.Store(true)
	f := file(1)
	a := r.appendWrite(t, f, 0, "a")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(a.Segment), 1, boom)); err != nil {
		t.Fatal(err)
	}
	r.appendWrite(t, f, 8, "b")
	if err := r.d.Sync(context.Background(), f); err == nil {
		t.Fatal("the failing sync returned nil")
	}
	eventually(t, "both spans to end", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(spans) == 2 && spans[0].end && spans[1].end
	})
	mu.Lock()
	defer mu.Unlock()
	if spans[0].op != core.SpanSync || spans[0].err != nil {
		t.Errorf("span 0 = %+v, want a sync span ended with nil", *spans[0])
	}
	if spans[1].op != core.SpanSync || !errors.Is(spans[1].err, boom) {
		t.Errorf("span 1 = %+v, want a sync span ended with the sync's error", *spans[1])
	}
}
