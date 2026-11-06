package durability

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// failRig is a started rig whose resolved windows land on resolved.
type failRig struct {
	*rig
	resolved chan core.SegmentID
}

// newFailRig builds a one-stream rig whose Resolved announcements are
// collected, runs pre on it before the layer starts, and starts it.
func newFailRig(t *testing.T, pre func(*failRig)) *failRig {
	t.Helper()
	fr := &failRig{resolved: make(chan core.SegmentID, 64)}
	fr.rig = newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(_ uint32, seg core.SegmentID) { fr.resolved <- seg }
	})
	if pre != nil {
		pre(fr)
	}
	fr.d.Start(fr.streams)
	return fr
}

// awaitResolved waits for one window to be announced resolved and returns its
// segment.
func (fr *failRig) awaitResolved(t *testing.T) core.SegmentID {
	t.Helper()
	select {
	case seg := <-fr.resolved:
		return seg
	case <-time.After(5 * time.Second):
		t.Fatal("no window was resolved")
		return 0
	}
}

// failSegmentOf fails the next sync of seg and leads it with a Sync of f,
// which returns the failure.
func (fr *failRig) failSegmentOf(t *testing.T, seg core.SegmentID, f core.FileID) {
	t.Helper()
	fr.failSyncOf(t, seg, errors.New("sync boom"))
	var se *core.SyncError
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)); !errors.As(err, &se) {
		t.Fatalf("Sync = %v, want a SyncError", err)
	}
}

// releaser returns a channel and the function that closes it once. It
// registers no cleanup: a caller whose hook waits on the channel passes the
// function to t.Cleanup itself, so a test that fails while the hook waits does
// not leave the worker stuck.
func releaser(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	closeIt := func() { once.Do(func() { close(ch) }) }
	return ch, closeIt
}

// events returns a copy of the loss events published so far, oldest first.
func (r *rig) events() []core.LossEvent {
	r.d.lmu.Lock()
	defer r.d.lmu.Unlock()
	out := make([]core.LossEvent, 0, r.d.count)
	for i := range r.d.count {
		out = append(out, r.d.ring[(r.d.head+i)%core.LossRingSize])
	}
	return out
}

// lossRecordsOnDisk returns the durable loss records' bodies, in segment order.
func (r *rig) lossRecordsOnDisk(t *testing.T) []format.Loss {
	t.Helper()
	var out []format.Loss
	for id := core.SegmentID(1); id < 32; id++ {
		raw, err := r.seam.Durable(format.SegmentName(id))
		if err != nil {
			continue
		}
		off := int64(format.SegmentHeaderSize)
		for off+format.RecordHeaderSize <= int64(len(raw)) {
			h, err := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], rigIdentity, id, off)
			if err != nil {
				break
			}
			end := off + format.RecordHeaderSize + h.BodySize()
			if end > int64(len(raw)) {
				t.Fatalf("record at %d of segment %d runs past the durable bytes", off, id)
			}
			body, err := format.VerifyRecordBody(raw[off+format.RecordHeaderSize:end], rigIdentity, h)
			if err != nil {
				t.Fatalf("record at %d of segment %d: %v", off, id, err)
			}
			if h.Kind == format.KindLoss {
				out = append(out, body.Loss)
			}
			off += h.Size()
		}
	}
	return out
}

// syncedThenOverwritten publishes a write of f at 0, syncs it, publishes a
// second one over it and returns the second record's place, version and
// sequence number, with the first's version.
func (r *rig) syncedThenOverwritten(t *testing.T, f core.FileID, first, second string) (loc segment.Location, v1, v2 core.Version, seq2 core.Seq) {
	t.Helper()
	_, v1 = r.publish(t, f, 0, first)
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of the first write: %v", err)
	}
	loc, v2 = r.publish(t, f, 0, second)
	return loc, v1, v2, core.Seq(r.lastSeq.Load())
}

// TestAFailedWindowServesTheLastSyncedWriteAndNamesTheLostOne covers the loss
// by exact record: the failed write leaves the index, the write it covered is
// served again, and one loss event and one durable loss record name it.
func TestAFailedWindowServesTheLastSyncedWriteAndNamesTheLostOne(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	loc, v1, v2, seq2 := fr.syncedThenOverwritten(t, f, "aaaa", "bbbb")

	fr.failSegmentOf(t, loc.Segment, f)
	if got := fr.awaitResolved(t); got != loc.Segment {
		t.Fatalf("resolved segment %d, want %d", got, loc.Segment)
	}

	p, ok := fr.ix.Lookup(f, 0)
	if !ok || p.Version != v1 {
		t.Fatalf("Lookup serves %+v, %v, want the synced v1", p, ok)
	}
	if got := fr.ix.Retained(f); len(got) != 0 {
		t.Fatalf("retained %+v after the window failed, want none", got)
	}
	evs := fr.events()
	if len(evs) != 1 {
		t.Fatalf("%d loss events, want 1: %+v", len(evs), evs)
	}
	if ev := evs[0]; ev.File != f || ev.Version != v2 || ev.Extent != (core.Extent{Off: 0, Len: 4}) || ev.Reason != core.LossFailedWindow || ev.Share != 1 {
		t.Fatalf("event = %+v, want v2's extent lost to a failed window", ev)
	}
	recs := fr.lossRecordsOnDisk(t)
	if len(recs) != 1 || recs[0].Dropped != seq2 || recs[0].Synced || recs[0].Reason != format.LossFailedSync {
		t.Fatalf("durable loss records = %+v, want one naming seq %d, unsynced", recs, seq2)
	}
}

// TestALossEventNamesTheExtentOfThePieceTheFailureDropped pins that the event
// carries the piece the window's record still held, not the record's header
// extent: a newer synced record covers the first half, so the loss is [4,8).
func TestALossEventNamesTheExtentOfThePieceTheFailureDropped(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	_, _ = fr.publish(t, f, 0, "aaaaaaaa")
	if err := fr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	loc, v10 := fr.publish(t, f, 0, "bbbbbbbb")
	v12, err := fr.d.counters.Draw()
	if err != nil {
		t.Fatal(err)
	}
	seq, err := fr.d.counters.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	placed := index.Placed{Extent: core.Extent{Off: 0, Len: 4}, Record: index.RecordAt{Segment: 99, Offset: 64}, Version: v12}
	unlock := fr.domains.Lock(f)
	if _, err := fr.ix.WriteRetain(1, f, placed, seq); err != nil {
		t.Fatal(err)
	}
	unlock()

	fr.failSegmentOf(t, loc.Segment, f)
	fr.awaitResolved(t)

	evs := fr.events()
	if len(evs) != 1 {
		t.Fatalf("%d loss events, want 1: %+v", len(evs), evs)
	}
	if ev := evs[0]; ev.Version != v10 || ev.Extent != (core.Extent{Off: 4, Len: 4}) {
		t.Fatalf("event = %+v, want v10's surviving piece [4,8), not its header extent [0,8)", ev)
	}
}

// TestAFailedWindowLeavesRecordsOfAnEarlierSegmentHeld pins that only the
// window's own records leave the index.
func TestAFailedWindowLeavesRecordsOfAnEarlierSegmentHeld(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	_, v1 := fr.publish(t, f, 0, "aaaa")
	if err := fr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	loc, v2 := fr.publish(t, f, 8, "bbbb")

	fr.failSegmentOf(t, loc.Segment, f)
	fr.awaitResolved(t)

	if p, ok := fr.ix.Lookup(f, 0); !ok || p.Version != v1 {
		t.Errorf("the synced record at 0 reads %+v, %v, want v1 still held", p, ok)
	}
	if p, ok := fr.ix.Lookup(f, 8); ok {
		t.Errorf("the failed record at 8 is still held: %+v (v2 = %v)", p, v2)
	}
}

// TestAFailedWindowRaisesTheGenerationOnce covers a window of many records
// over several files: one raise, and one event per record.
func TestAFailedWindowRaisesTheGenerationOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	before := fr.d.Generation()
	var first segment.Location
	for i := range 50 {
		loc, _ := fr.publish(t, file(byte(1+i%5)), int64(i*4), "xxxx")
		if i == 0 {
			first = loc
		}
	}
	fr.failSegmentOf(t, first.Segment, file(1))
	fr.awaitResolved(t)

	if got := fr.d.Generation() - before; got != 1 {
		t.Fatalf("the generation rose by %d for one window, want 1", got)
	}
	if got := len(fr.events()); got != 50 {
		t.Fatalf("%d loss events for 50 failed records", got)
	}
}

// TestTheGenerationIsRaisedBeforeTheFirstLossIsPublished pins the order: a
// reader that sees a loss event finds the generation already raised.
func TestTheGenerationIsRaisedBeforeTheFirstLossIsPublished(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var seen []uint64
	var mu sync.Mutex
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.beforePublish = func() {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, fr.d.Generation())
		}
	})
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	fr.publish(t, f, 8, "bbbb")
	before := fr.d.Generation()
	fr.failSegmentOf(t, loc.Segment, f)
	fr.awaitResolved(t)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("beforePublish ran %d times, want 2", len(seen))
	}
	for i, g := range seen {
		if g != before+1 {
			t.Errorf("loss %d found generation %d when published, want %d", i, g, before+1)
		}
	}
}

// TestAWriterThatPublishesAfterTheSyncFailedDoesNotBringTheRecordBack covers
// the gap between a writer's append and its publish: the failure is handled
// only after the writer, which holds the file's domain, published.
func TestAWriterThatPublishesAfterTheSyncFailedDoesNotBringTheRecordBack(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f, g := file(1), file(2)
	_, v1 := fr.publish(t, f, 0, "aaaa")
	fr.publish(t, g, 0, "gggg")
	if err := fr.d.Sync(context.Background(), f, g); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	release, open := releaser(t)
	t.Cleanup(open)
	published := make(chan struct{})
	taken := func() int {
		fr.domains.mu.Lock()
		defer fr.domains.mu.Unlock()
		return fr.domains.taken
	}
	go func() {
		defer close(published)
		fr.publishGap(t, f, 0, "bbbb", func() { <-release })
	}()
	// The writer holds f's domain once its append is in; learn the segment
	// from the ledger.
	var seg core.SegmentID
	eventually(t, "the writer's record to be tracked", func() bool {
		st := fr.d.made[0]
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, e := range st.ledger {
			if e.w.File == f && !st.covered(e.w.Loc) {
				seg = e.w.Loc.Segment
				return true
			}
		}
		return false
	})
	fr.publish(t, g, 8, "hhhh")
	fr.failSyncOf(t, seg, errors.New("sync boom"))
	before := taken()
	_ = fr.syncAsync(context.Background(), g)
	eventually(t, "the worker to ask for f's domain", func() bool { return taken() > before })
	open()
	<-published
	fr.awaitResolved(t)

	if p, ok := fr.ix.Lookup(f, 0); !ok || p.Version != v1 {
		t.Fatalf("Lookup serves %+v, %v, want the synced v1: the failed record came back", p, ok)
	}
}

// TestALossRecordIsNotAppendedUnderAFileDomain pins that a read never waits
// on a loss record's append: the domain and the index are free while the first
// loss record is blocked at the storage.
func TestALossRecordIsNotAppendedUnderAFileDomain(t *testing.T) {
	prof.Watch(t, prof.Options{})
	atSync := make(chan struct{}, 1)
	release, open := releaser(t)
	fr := newFailRig(t, func(fr *failRig) {
		t.Cleanup(open)
		fr.d.beforeSync = func() {
			select {
			case atSync <- struct{}{}:
			default:
			}
			<-release
		}
	})
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	for i := 1; i < 4; i++ {
		fr.publish(t, f, int64(i*4), "aaaa")
	}
	fr.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	answer := fr.syncAsync(context.Background(), f)
	select {
	case <-atSync:
	case <-time.After(5 * time.Second):
		t.Fatal("no loss record reached its sync")
	}

	got := make(chan struct{})
	go func() {
		unlock := fr.domains.Lock(f)
		fr.ix.Lookup(f, 0)
		unlock()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the file's domain is held while a loss record is appended")
	}
	// The failure is not answered while the loss is being recorded.
	mustBlock(t, "Sync while a loss record is appended", answer)
	open()
	wantSyncError(t, mustReturn(t, "Sync", answer), loc.Segment, f)
	fr.awaitResolved(t)
}

// TestFailingAWindowAccountsWhatTheIndexMoved pins that the deltas handed to
// the account sum to the change in the segment's held and retained bytes.
func TestFailingAWindowAccountsWhatTheIndexMoved(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	_, _, _, _ = fr.syncedThenOverwritten(t, f, "aaaa", "bbbbbbbb")
	seg := fr.ix.Retained(f)[0].Record.Segment
	heldBefore, retBefore := fr.ix.SegmentBytes(seg).Held, fr.ix.SegmentBytes(seg).Retained
	n := len(fr.acct.all())

	fr.failSegmentOf(t, seg, f)
	fr.awaitResolved(t)

	var held, ret int64
	for _, hd := range fr.acct.all()[n:] {
		held += hd.HeldBytes
		ret += hd.RetainedBytes
	}
	if want := fr.ix.SegmentBytes(seg).Held - heldBefore; held != want || want != -4 {
		t.Errorf("accounted held change %d, the segment's %d, want -4", held, want)
	}
	if want := fr.ix.SegmentBytes(seg).Retained - retBefore; ret != want || want != -4 {
		t.Errorf("accounted retained change %d, the segment's %d, want -4", ret, want)
	}
}

// TestAFailedWindowLiftsWhatANewerSyncedRecordHeld covers the cover: a newer
// record that already synced is lifted when the older one is failed, and one
// still in the ledger is left to its own completion.
func TestAFailedWindowLiftsWhatANewerSyncedRecordHeld(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	loc, _, v2, _ := fr.syncedThenOverwritten(t, f, "aaaa", "bbbb")
	// A newer record over the first half that no ledger entry names: it
	// synced and completed long ago.
	v12, err := fr.d.counters.Draw()
	if err != nil {
		t.Fatal(err)
	}
	seq, err := fr.d.counters.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	placed := index.Placed{Extent: core.Extent{Off: 0, Len: 2}, Record: index.RecordAt{Segment: 99, Offset: 64}, Version: v12}
	unlock := fr.domains.Lock(f)
	if _, err := fr.ix.WriteRetain(1, f, placed, seq); err != nil {
		t.Fatal(err)
	}
	unlock()
	if len(fr.retainedBy(f, v12)) == 0 {
		t.Fatalf("v12 retained nothing before the failure: %+v (v2 = %v)", fr.ix.Retained(f), v2)
	}

	fr.failSegmentOf(t, loc.Segment, f)
	fr.awaitResolved(t)

	if got := fr.retainedBy(f, v12); len(got) != 0 {
		t.Fatalf("%+v is still retained under the synced v12", got)
	}
	if p, ok := fr.ix.Lookup(f, 2); !ok || p.Extent.Len == 0 {
		t.Fatalf("the second half reads %+v, %v, want the synced first write again", p, ok)
	}
}

// TestAFailedWindowLeavesWhatARecordStillInTheLedgerHeld pins the other half of
// the cover: a newer record whose sync has not completed keeps what it
// superseded, since it can still fail.
func TestAFailedWindowLeavesWhatARecordStillInTheLedgerHeld(t *testing.T) {
	prof.Watch(t, prof.Options{})
	// A loss record's sync also covers the newer record, which sits in the
	// stream's next segment, so the check is made while f's own loss record,
	// the second, waits for it: f's records are failed by then and nothing has
	// completed yet.
	atLoss := make(chan struct{}, 1)
	release, open := releaser(t)
	var losses atomic.Int32
	fr := newFailRig(t, func(fr *failRig) {
		t.Cleanup(open)
		fr.d.beforeSync = func() {
			if losses.Add(1) < 2 {
				return
			}
			select {
			case atLoss <- struct{}{}:
			default:
			}
			<-release
		}
	})
	f, h := file(1), file(2)
	fr.publish(t, h, 0, "hhhh")
	fr.publish(t, f, 0, "aaaa")
	if err := fr.d.Sync(context.Background(), f, h); err != nil {
		t.Fatal(err)
	}
	loc, _ := fr.publish(t, h, 8, "hhhh")
	fr.publish(t, f, 0, "bbbb")

	// The worker fails h's records first and waits on h's domain, which the
	// test holds while it writes f's newer record in the segment after.
	unlockH := fr.domains.Lock(h)
	fr.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	fr.domains.mu.Lock()
	taken := fr.domains.taken
	fr.domains.mu.Unlock()
	told := fr.syncAsync(context.Background(), f)
	eventually(t, "the worker to wait on h's domain", func() bool {
		fr.domains.mu.Lock()
		defer fr.domains.mu.Unlock()
		return fr.domains.taken > taken
	})
	fr.settlePool(t)
	_, v12 := fr.publish(t, f, 0, "cc")
	unlockH()
	select {
	case <-atLoss:
	case <-time.After(5 * time.Second):
		t.Fatal("no loss record was appended")
	}

	if got := fr.retainedBy(f, v12); len(got) == 0 {
		t.Fatalf("the record of v12 lost what it superseded before its own sync: %+v", fr.ix.Retained(f))
	}
	open()
	fr.awaitResolved(t)
	// The waiting Sync is the one told of f's failed records.
	wantSyncError(t, mustReturn(t, "Sync of the failed file", told), loc.Segment, f)
	if err := fr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v12: %v", err)
	}
	if got := fr.retainedBy(f, v12); len(got) != 0 {
		t.Fatalf("%+v is still retained after v12 synced", got)
	}
}

// TestAWindowWithAHeaderOnlyRecordStaysPending pins the interim: only content
// records are failed, so a window holding another kind is not resolved and
// keeps that record.
func TestAWindowWithAHeaderOnlyRecordStaysPending(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	loc := fr.appendWrite(t, f, 0, "aaaa")
	fr.appendKind(t, f, format.KindDelete, format.RecordBody{})
	fr.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	_ = fr.syncAsync(context.Background(), f)

	// The write is failed and named; the delete keeps the window.
	eventually(t, "the content record to be failed", func() bool { return len(fr.events()) == 1 })
	eventually(t, "the window to hold only the delete", func() bool {
		ws := fr.windowsOf(0)
		return len(ws) == 1 && len(ws[0].locs) == 1
	})
	select {
	case seg := <-fr.resolved:
		t.Fatalf("segment %d was resolved with a header-only record pending", seg)
	case <-time.After(100 * time.Millisecond):
	}
	if fr.d.Durable(loc) {
		t.Error("a failed record reads durable")
	}
}

// TestAResolvedWindowIsAnnouncedOnce covers the announcement: once, with the
// failed segment, after the last content record is failed.
func TestAResolvedWindowIsAnnouncedOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	fr.publish(t, f, 8, "bbbb")
	fr.failSegmentOf(t, loc.Segment, f)
	if got := fr.awaitResolved(t); got != loc.Segment {
		t.Fatalf("resolved segment %d, want %d", got, loc.Segment)
	}
	select {
	case seg := <-fr.resolved:
		t.Fatalf("a second announcement, of segment %d", seg)
	case <-time.After(100 * time.Millisecond):
	}
	if len(fr.windowsOf(0)) != 0 {
		t.Error("the resolved window is still pending")
	}
	if fr.d.Durable(loc) {
		t.Error("a failed record reads durable once its window is resolved")
	}
}

// TestFailingAWindowHoldsOneFileDomainAtATime pins the lock order: the domains
// of a window's files are taken one after the other, and none is held while a
// loss record is appended.
func TestFailingAWindowHoldsOneFileDomainAtATime(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var mu sync.Mutex
	var heldAtLose []int
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.afterAppend = func() {
			mu.Lock()
			defer mu.Unlock()
			heldAtLose = append(heldAtLose, fr.domains.heldNow())
		}
	})
	var first segment.Location
	for i := range 6 {
		loc, _ := fr.publish(t, file(byte(1+i)), 0, "aaaa")
		if i == 0 {
			first = loc
		}
	}
	fr.failSegmentOf(t, first.Segment, file(1))
	fr.awaitResolved(t)

	fr.domains.mu.Lock()
	most := fr.domains.mostHeld
	fr.domains.mu.Unlock()
	if most != 1 {
		t.Errorf("%d file domains were held at once, want 1", most)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heldAtLose) != 6 || slices.ContainsFunc(heldAtLose, func(n int) bool { return n != 0 }) {
		t.Errorf("domains held at each loss record's append: %v, want six zeros", heldAtLose)
	}
}
