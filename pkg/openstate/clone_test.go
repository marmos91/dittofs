package openstate

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// cloned runs a clone's first phase of src's first 4 KiB onto dst, as the
// engine does, and answers the clone it holds src for.
func (f *fixture) cloned(t *testing.T, src, dst FileKey) CloneSource {
	t.Helper()
	ex := f.fs.Store.Existence()
	failed, err := ex.Commit(ctx, []kvstore.PendingExistence{{Share: src.Share, File: src.ID,
		Ops: []kvstore.PendingOp{{Kind: kvstore.OpWrite, Version: entity.VersionOf(1), Extent: entity.ExtentOf(0, 4096)}}}}, nil)
	if err == nil {
		err = failed[src.ID]
	}
	if err == nil {
		err = ex.Clone(ctx, kvstore.CloneOp{Share: dst.Share, File: dst.ID, Version: entity.VersionOf(5),
			Spec: entity.CloneSpec{SrcShare: src.Share, Src: src.ID, Len: 4096}}, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return CloneSource{Dst: dst.ID, Version: entity.VersionOf(5)}
}

// lastBatch runs the clone onto dst to its end, whose last batch deletes its
// hold on the source.
func (f *fixture) lastBatch(t *testing.T, dst FileKey) {
	t.Helper()
	if err := f.fs.Store.Content().Resume(ctx, dst.Share, dst.ID); err != nil {
		t.Fatal(err)
	}
}

// A clone's hold on its source is an open of no client, held from CloneOpen
// to CloneClose: no client uses or closes it, it defers no delete, it keeps
// an unlinked source alive past its last client's close, and its close
// reports the source an orphan.
func TestCloneHoldsItsSource(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	src, r := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	c := f.cloned(t, src, dst)
	if _, err := f.Opens().CloneOpen(ctx, FileKey{Share: src.Share, Shard: 3, ID: src.ID}, c); delayedFor(err) != DelayFrozen {
		t.Errorf("CloneOpen in a shard not begun: %v, want ErrDelay (DelayFrozen)", err)
	}
	if _, err := f.Opens().CloneOpen(ctx, src, CloneSource{Dst: dst.ID, Version: entity.VersionOf(4)}); !errors.Is(err, ErrBadState) {
		t.Errorf("CloneOpen of a clone with no hold: %v, want ErrBadState", err)
	}
	id, err := f.Opens().CloneOpen(ctx, src, c)
	if got := f.stored(t, src); err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("CloneOpen %d, %v; stored %+v", id, err, got)
	}
	if again, err := f.Opens().CloneOpen(ctx, src, c); err != nil || again != id {
		t.Errorf("CloneOpen again %d, %v; want the same hold", again, err)
	}
	if _, err := f.Opens().Close(ctx, Caller{}, OpenRef{Kind: RefOpened, Open: id}, Seqs{}); !errors.Is(err, ErrBadState) {
		t.Errorf("a Close naming the hold: %v, want ErrBadState", err)
	}

	r.Access, r.DeleteOnClose = rd|del, true
	oa := f.mustOpen(t, a, src, r)
	if err := f.Opens().CloneClose(ctx, oa.ID); !errors.Is(err, ErrBadState) {
		t.Errorf("CloneClose of a client's open: %v, want ErrBadState", err)
	}
	if res := f.close(t, a, oa); res.Unlink == nil {
		t.Fatalf("the last client close of a delete-on-close source: %+v, want its unlink", res)
	}
	f.unlink(t, src, "src")
	oa = f.mustOpen(t, a, src, OpenRequest{Access: rd})
	if res := f.close(t, a, oa); res.Orphan {
		t.Error("the last client close of an unlinked source the clone holds reported an orphan")
	}
	f.lastBatch(t, dst)
	if got := f.stored(t, src); len(got) != 0 {
		t.Errorf("stored after the last batch %+v, want the hold gone", got)
	}
	f.sink.mu.Lock()
	f.sink.closed = nil
	f.sink.mu.Unlock()
	if err := f.Opens().CloneClose(ctx, id); err != nil {
		t.Fatal(err)
	}
	f.sink.mu.Lock()
	got := slices.Clone(f.sink.closed)
	f.sink.mu.Unlock()
	if len(got) != 1 || got[0].f != src.ID || !got[0].r.Orphan {
		t.Errorf("CloneClose raised Closed %+v; want the unlinked source reported an orphan, once", got)
	}
	if err := f.Opens().CloneClose(ctx, id); !errors.Is(err, ErrBadState) {
		t.Errorf("CloneClose again: %v, want ErrBadState", err)
	}
}

// A clone's hold is installed with its shard and kept through grace's end,
// held or not yet held here; only CloneClose ends it.
func TestCloneHoldOutlivesGrace(t *testing.T) {
	f := newFixture(t)
	src, _ := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	unheld, _ := f.fileIn(t, "unheld")
	udst, _ := f.fileIn(t, "udst")
	id, err := f.Opens().CloneOpen(ctx, src, f.cloned(t, src, dst))
	if err != nil {
		t.Fatal(err)
	}
	g := f.restart(t)
	f.cloned(t, unheld, udst)
	g.clock.mu.Lock()
	g.clock.now += leasePeriod + time.Second
	g.clock.mu.Unlock()
	g.closeUnreclaimed(ctx, g.sink)
	if len(g.stored(t, src)) != 1 || len(g.stored(t, unheld)) != 1 {
		t.Fatalf("grace's end closed a clone's hold: %+v, %+v", g.stored(t, src), g.stored(t, unheld))
	}
	g.lastBatch(t, dst)
	if err := g.Opens().CloneClose(ctx, id); err != nil {
		t.Errorf("CloneClose after a restart: %v", err)
	}
}

// An unlink of the source between the clone's last batch and CloneClose does
// not write the hold's record back: the hold's record is the clone's, and
// nothing holds the unlinked source after CloneClose, nor after a restart.
func TestUnlinkAfterLastBatchLeavesNoHold(t *testing.T) {
	f := newFixture(t)
	src, _ := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	id, err := f.Opens().CloneOpen(ctx, src, f.cloned(t, src, dst))
	if err != nil {
		t.Fatal(err)
	}
	f.lastBatch(t, dst)
	f.unlink(t, src, "src")
	if err := f.Opens().CloneClose(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := f.stored(t, src); len(got) != 0 {
		t.Errorf("the unlinked source's records after CloneClose: %+v, want none", got)
	}
	g := f.restart(t)
	g.clock.mu.Lock()
	g.clock.now += leasePeriod + time.Second
	g.clock.mu.Unlock()
	g.closeUnreclaimed(ctx, g.sink)
	if got := g.stored(t, src); len(got) != 0 {
		t.Errorf("after a restart and grace's end: %d records still hold the unlinked source", len(got))
	}
}

// A clone's hold denies nothing: an SMB rename replacing its source is
// admitted while the clone runs.
func TestCloneHoldAdmitsReplaceOfItsSource(t *testing.T) {
	f := newFixture(t)
	s := f.caller(t, smb("pc-a"))
	src, _ := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	if _, err := f.Opens().CloneOpen(ctx, src, f.cloned(t, src, dst)); err != nil {
		t.Fatal(err)
	}
	if err := f.Checks().CheckChange(ctx, s, src, nil, ChangeRename|ChangeReplace); err != nil {
		t.Errorf("an SMB rename replacing a source only a clone holds: %v, want nil", err)
	}
}

// heldWaiter holds src for a clone onto dst and queues h's callback waiter on
// src behind a poll waiter's lapsed reservation, so serving src's waiters
// grants h, and runs the clone to its last batch. It answers the hold.
func (f *fixture) heldWaiter(t *testing.T) (FileKey, entity.OpenID) {
	t.Helper()
	a, b, h := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.host(t, "b", 1), f.host(t, "h", 1)
	src, r := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	id, err := f.Opens().CloneOpen(ctx, src, f.cloned(t, src, dst))
	if err != nil {
		t.Fatal(err)
	}
	r.Access = rd | wr
	o := f.mustOpen(t, a, src, r)
	f.mustLock(t, b, src, LockReq{Owner: owner(b, "b"), Ranges: []LockRange{x(0, 10)}})
	if _, err := f.lock(a, src, LockReq{Owner: owner(a, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 10)}, Wait: WaitPoll}); !errors.Is(err, ErrLocked) {
		t.Fatalf("lo's poll: %v, want ErrLocked", err)
	}
	f.mustLock(t, h, src, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.unlock(t, b, src, owner(b, "b"), br(0, 10))
	if ws := f.sink.takeWaits(); len(ws) != 0 {
		t.Fatalf("the range freed under lo's poll granted %+v, want it reserved", ws)
	}
	f.clock.mu.Lock()
	f.clock.now += leasePeriod + time.Second // lo's reservation lapses, with no sweep run
	f.clock.mu.Unlock()
	f.lastBatch(t, dst)
	f.order.mu.Lock()
	f.order.files = nil
	f.order.mu.Unlock()
	return src, id
}

// grantedWithin waits up to d for the sink to report grants, and answers
// them.
func (f *fixture) grantedWithin(t *testing.T, d time.Duration) []string {
	t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(time.Millisecond) {
		if ws := f.sink.takeWaits(); len(ws) != 0 || time.Now().After(end) {
			return granted(t, ws)
		}
	}
}

// CloneClose names a file but is called holding no admission lock, so the
// file's waiters are served under it: a callback waiter queued behind a poll
// waiter's lapsed reservation is granted, with the file's admission lock
// taken.
func TestCloneCloseServesWaitersUnderOrder(t *testing.T) {
	f := newFixture(t)
	src, id := f.heldWaiter(t)
	if err := f.Opens().CloneClose(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := f.grantedWithin(t, time.Second); len(got) != 1 || got[0] != "h" {
		t.Fatalf("after CloneClose: granted %v, want h", got)
	}
	if !f.ordered(src) {
		t.Error("CloneClose granted the waiter without the file's admission lock")
	}
}

// CloneClose returns once the hold is closed, and the freed file is served
// after the clone is answered (RFC 14 L70): with the source's admission lock
// held across the answer, as the clone's call holds it, CloneClose returns at
// once and grants nothing until the lock is let go, then the waiter is
// granted.
func TestCloneCloseServesTheSourceAfterTheAnswer(t *testing.T) {
	f := newFixture(t)
	src, id := f.heldWaiter(t)
	answer := f.order.order(src.ID) // the clone's call, holding the source until it is answered
	closed := make(chan error, 1)
	go func() { closed <- f.Opens().CloneClose(ctx, id) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		answer()
		t.Fatal("CloneClose did not return within 1s while the clone's call held the source")
	}
	if got := f.grantedWithin(t, 50*time.Millisecond); len(got) != 0 {
		t.Fatalf("before the clone is answered: granted %v, want nothing", got)
	}
	answer()
	if got := f.grantedWithin(t, time.Second); len(got) != 1 || got[0] != "h" {
		t.Fatalf("after the answer: granted %v within 1s, want h", got)
	}
}

// Stop joins the serving a CloneClose left to run: it returns only once the
// freed file has been served.
func TestStopJoinsACloneClosesServing(t *testing.T) {
	f := newFixture(t)
	src, id := f.heldWaiter(t)
	answer := f.order.order(src.ID)
	if err := f.Opens().CloneClose(ctx, id); err != nil {
		answer()
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { f.Stop(); close(stopped) }()
	select {
	case <-stopped:
		answer()
		t.Fatal("Stop returned with the freed file's serving still waiting")
	case <-time.After(50 * time.Millisecond):
	}
	answer()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return within 1s of the serving's lock being let go")
	}
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "h" {
		t.Errorf("by Stop's return: granted %v, want h", got)
	}
}

// Served returns once ServeShard has installed the shard, at once for one
// already served, and with ctx's error when ctx ends first.
func TestServedWaitsForServeShard(t *testing.T) {
	f := newFixture(t)
	if err := f.Recovery().Served(ctx, testShards[0]); err != nil {
		t.Errorf("Served of a shard already served: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- f.Recovery().Served(ctx, 9) }()
	select {
	case err := <-served:
		t.Fatalf("Served before ServeShard returned %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := f.Recovery().ServeShard(ctx, f.fs.Share.ID, 9, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Served after ServeShard: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Served did not return within 1s of ServeShard")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.Recovery().Served(cctx, 11); !errors.Is(err, context.Canceled) {
		t.Errorf("Served of a shard never served, ctx cancelled: %v, want context.Canceled", err)
	}
}
