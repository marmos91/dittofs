package openstate

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// anon is an anonymous read of one byte at off of file, as NFSv3 reads.
func (f *fixture) anon(file FileKey, off uint64) error {
	return f.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, State: StateRef{Shard: file.Shard}, Handle: handle(file.ID)}, br(off, 1), false)
}

// A lock a persistent open's callback waiter is granted is written before
// it is granted, so it survives a restart; a refused write answers the
// waiter not granted, holding nothing.
func TestWaiterLockOfPersistentOpenIsWritten(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	oa := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, 300*time.Second, 1))
	wait := func(rg LockRange) Open {
		ob := f.mustOpen(t, b, file, OpenRequest{Access: rd})
		f.mustLock(t, b, file, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{rg}})
		if res, err := f.lock(a, file, LockReq{Owner: smbOwner(a, oa), Open: byID(oa), Ranges: []LockRange{rg}, Wait: WaitCallback}); err != nil || !res.Waiting {
			t.Fatalf("a waits: %+v %v", res, err)
		}
		return ob
	}

	ob := wait(x(20, 10))
	st.fail.Store(true)
	f.close(t, b, ob)
	st.fail.Store(false)
	if ws := f.sink.takeWaits(); len(ws) != 1 || ws[0].granted {
		t.Errorf("the waiter whose lock's write was refused: %+v, want it answered not granted", ws)
	}
	if h := f.holder(t, file, br(20, 10)); h != nil {
		t.Errorf("the refused waiter's range is held: %+v", h)
	}

	f.close(t, b, wait(x(0, 10)))
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 {
		t.Fatalf("granted %v", got)
	}
	g := f.restart(t)
	g.renew(t, b)
	g.idle(120 * time.Second)
	if err := g.anon(file, 5); !errors.Is(err, ErrLocked) {
		t.Errorf("an anonymous read across a's granted lock after a restart: %v, want ErrLocked", err)
	}
}

// A callback's refusal of a lock granted to a persistent open releases it
// in the store too.
func TestWaiterRefusalOfPersistentOpenIsWritten(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	oa := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, 300*time.Second, 1))
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd})
	f.mustLock(t, b, file, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{x(0, 10)}})
	if _, err := f.lock(a, file, LockReq{Owner: smbOwner(a, oa), Open: byID(oa), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); err != nil {
		t.Fatal(err)
	}
	f.close(t, b, ob)
	ws := f.sink.takeWaits()
	if len(ws) != 1 || !ws[0].granted {
		t.Fatalf("waits %+v", ws)
	}
	if err := f.Locks().WaitAnswered(ctx, a, ws[0].w, true); err != nil {
		t.Fatal(err)
	}
	g := f.restart(t)
	g.renew(t, b)
	g.idle(120 * time.Second)
	if err := g.anon(file, 5); err != nil {
		t.Errorf("an anonymous read across the range the refusal released, after a restart: %v", err)
	}
}

// Once a session has ended no open is attached to it: an open naming it is
// ErrStaleClient and a reconnect onto it ErrNoReclaim, for a lease period.
func TestNoOpenAttachesToAnEndedSession(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	req := keep(OpenRequest{Access: rd, Deny: wr, Want: rwh}, entity.DurabilityDurable, s1, time.Minute, 1)
	o := f.mustOpen(t, a, file, req)
	f.disconnect(t, a, s1)
	f.disconnect(t, a, s2)
	if _, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s2); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect onto an ended session: %v, want ErrNoReclaim", err)
	}
	onto := keep(OpenRequest{Access: rd}, entity.DurabilityNone, s2, 0, 0)
	if _, err := f.open(a, anyFile(), onto); !errors.Is(err, ErrStaleClient) {
		t.Errorf("an open onto an ended session: %v, want ErrStaleClient", err)
	}
	if _, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s3); err != nil {
		t.Errorf("a reconnect onto a live session: %v", err)
	}
	f.clock.advance(leasePeriod)
	f.sweep(ctx)
	if _, err := f.open(a, anyFile(), onto); err != nil {
		t.Errorf("an open naming the session a lease period after its end: %v", err)
	}
}

// Parking a durable open drops its waiters, a callback one answered not
// granted, so none is granted to an open no session holds. The lock it waits
// on is its own client's, under its lease key, as another holder's lock would
// keep the lease from it.
func TestWaiterOfParkedOpenGoes(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	o2 := f.mustOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}})
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o2), Open: byID(o2), Ranges: []LockRange{x(0, 10)}})
	oa := f.mustOpen(t, a, file, keep(OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh}, entity.DurabilityDurable, s1, time.Minute, 1))
	if res, err := f.lock(a, file, LockReq{Owner: smbOwner(a, oa), Open: byID(oa), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); err != nil || !res.Waiting {
		t.Fatalf("a waits: %+v %v", res, err)
	}
	f.disconnect(t, a, s1)
	if ws := f.sink.takeWaits(); len(ws) != 1 || ws[0].granted {
		t.Errorf("the parked open's waiter: %+v, want it answered not granted", ws)
	}
	if _, err := f.Locks().Unlock(ctx, a, file, byID(o2), smbOwner(a, o2), []ByteRange{br(0, 10)}, Seqs{}); err != nil {
		t.Fatal(err)
	}
	if h := f.holder(t, file, br(0, 10)); h != nil {
		t.Errorf("a disconnected open's waiter was granted the range: %+v", h)
	}
}

// A durable open whose lease a break took handle caching from is closed at
// its disconnect.
func TestDurableWithoutHandleClosesAtDisconnect(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	req := keep(OpenRequest{Access: rd, Deny: wr, Lease: LeaseKey{7}, Want: rh}, entity.DurabilityDurable, s1, time.Minute, 1)
	_, g := f.grantOpen(t, a, file, req)
	if _, err := f.open(b, file, OpenRequest{Access: wr}); delayedFor(err) != DelayRecall {
		t.Fatalf("b: %v", err)
	}
	rc := f.sink.takeRecalls()
	if err := f.ack(a, g, rc[0].to); err != nil {
		t.Fatal(err)
	}
	f.disconnect(t, a, s1)
	if _, err := f.open(b, file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open after the disconnect of a durable open holding no handle caching: %v", err)
	}
}

// A break of a grant covering only disconnected durable opens closes them
// rather than recalling a client no session is left to answer for; one
// covering a persistent open is recalled.
func TestBreakClosesAParkedDurableOpen(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	o := f.mustOpen(t, a, file, keep(OpenRequest{Access: rd, Deny: wr, Want: rwh}, entity.DurabilityDurable, s1, time.Minute, 1))
	f.disconnect(t, a, s1)
	if _, err := f.open(b, file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a conflicting open against a disconnected durable open: %v, want it granted", err)
	}
	if rc := f.sink.takeRecalls(); len(rc) != 0 {
		t.Errorf("recalls %+v, want none", rc)
	}
	if _, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s2); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect of the closed open: %v, want ErrNoReclaim", err)
	}

	pf, r := f.fileIn(t, "disk")
	r.Access, r.Want = rd, rwh
	f.mustOpen(t, a, pf, keep(r, entity.DurabilityPersistent, s2, time.Minute, 2))
	f.disconnect(t, a, s2)
	if _, err := f.open(b, pf, OpenRequest{Access: wr}); delayedFor(err) != DelayRecall {
		t.Errorf("a conflicting open against a disconnected persistent open: %v, want its grant recalled", err)
	}
}

// A store refusing an acknowledgement's write answers its error and changes
// nothing; the acknowledgement is then taken and written.
func TestRefusedAckChangesNothing(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Want = rd|wr, LeaseKey{5}, rwh
	res, err := f.Opens().Open(ctx, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	if err != nil || res.Grant == nil {
		t.Fatal(res, err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	to := f.sink.takeRecalls()[0].to
	st.fail.Store(true)
	err = f.ack(a, res.Grant, to)
	st.fail.Store(false)
	if !errors.Is(err, errWrite) {
		t.Errorf("an acknowledgement over a refusing store: %v, want its error", err)
	}
	if err := f.ack(a, res.Grant, to); err != nil {
		t.Errorf("the acknowledgement again: %v, want the recall still in flight", err)
	}
}

// A recall's timeout over a refusing store leaves the lease's persistent
// open unsaved: the timer writes it again on each sweep until a write
// passes, and meanwhile the file is offered no grant.
func TestTimedOutLeaseIsWrittenAgain(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Want = rd, LeaseKey{5}, rwh
	res, err := f.Opens().Open(ctx, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	if err != nil || res.Grant == nil {
		t.Fatal(res, err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	st.fail.Store(true)
	f.pass(t, coversDeadline+time.Second)
	f.idle(0) // the timer's write is refused again
	st.fail.Store(false)
	f.clock.advance(f.recalledFor(file))
	if res, err := f.Opens().Open(ctx, b, file, OpenRequest{Access: rd, Want: GrantRead}); err != nil || res.Grant != nil {
		t.Errorf("an open on the unsaved file: grant %+v, %v; want none offered", res.Grant, err)
	}
	f.idle(0)
	if res, err := f.Opens().Open(ctx, b, file, OpenRequest{Access: rd, Want: GrantRead}); err != nil || res.Grant == nil {
		t.Errorf("an open once the record is written again: grant %+v, %v; want one offered", res.Grant, err)
	}
	g := f.restart(t)
	m := ReconnectMatch{Share: file.Share, Create: [16]byte{1}, Lease: LeaseKey{5}}
	back, err := g.reconnect(a, res.Open, m, s2)
	if err != nil || back.Grant == nil || back.Grant.Kind != GrantNone {
		t.Errorf("reconnected under %+v, %v; want the lease at none", back.Grant, err)
	}
}

// A committed rename over a refusing store is answered, its open left
// unsaved for the timer to write again, after which a restart installs the
// new name.
func TestRenameIsWrittenAgain(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	st.fail.Store(true)
	err := f.Opens().Renamed(ctx, file, r.Via, r.Name, r.Via, []byte("moved"))
	st.fail.Store(false)
	if err != nil {
		t.Errorf("Renamed over a refusing store: %v, want the committed rename answered", err)
	}
	f.disconnect(t, a, s1)
	m := ReconnectMatch{Share: file.Share, Create: [16]byte{1}}
	if _, err := f.reconnect(a, o, m, s2); !errors.Is(err, ErrDelay) {
		t.Errorf("a reconnect of the unsaved open: %v, want ErrDelay", err)
	}
	f.idle(0)
	g := f.restart(t)
	if back, err := g.reconnect(a, o, m, s2); err != nil || string(back.Open.Name) != "moved" {
		t.Errorf("reconnected %+v, %v; want the new name", back.Open, err)
	}
}

// recalledFor is how long file is offered no grant after a recall ended.
func (f *fixture) recalledFor(file FileKey) time.Duration {
	f.idx.Lock()
	defer f.idx.Unlock()
	return f.recalled[file.ID] - f.clock.Elapsed() + time.Second
}

// An acknowledgement between an unlink's snapshot of the records and its
// commit is not lost to the snapshot the unlink wrote.
func TestUnlinkKeepsALaterAcknowledgement(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Want = rd|wr, LeaseKey{5}, rwh
	res, err := f.Opens().Open(ctx, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	if err != nil || res.Grant == nil {
		t.Fatal(res, err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	to := f.sink.takeRecalls()[0].to
	recs, commit, err := f.Opens().Durable(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ack(a, res.Grant, to); err != nil {
		t.Fatal(err)
	}
	o, err := f.fs.Store.Namespace().Unlink(ctx, f.fs.Admin, f.fs.Root, []byte("doc"), recs...)
	commit(err == nil && o.File == file.ID)
	if err != nil {
		t.Fatal(err)
	}
	g := f.restart(t)
	back, err := g.reconnect(a, res.Open, ReconnectMatch{Share: file.Share, Create: [16]byte{1}, Lease: LeaseKey{5}}, s2)
	if err != nil || back.Grant == nil || back.Grant.Kind != to {
		t.Errorf("reconnected under %+v, %v; want the lease at %v", back.Grant, err, to)
	}
}

// Locks taken under a persistent open in separate requests are each kept.
func TestPersistentLocksAccumulate(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, 300*time.Second, 1))
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(20, 10)}})
	g := f.restart(t)
	g.renew(t, b)
	g.idle(120 * time.Second)
	for _, off := range []uint64{5, 25} {
		if err := g.anon(file, off); !errors.Is(err, ErrLocked) {
			t.Errorf("an anonymous read at %d after a restart: %v, want ErrLocked", off, err)
		}
	}
}

// A refused lock or unlock under a persistent open changes nothing: an
// unlock that releases nothing writes nothing, and a lock whose write fails
// keeps the owner's earlier waiter.
func TestRefusedLockRequestChangesNothing(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	oa := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, oa), Open: byID(oa), Ranges: []LockRange{x(50, 10)}})
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd})
	f.mustLock(t, b, file, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{x(0, 10)}})
	if res, err := f.lock(a, file, LockReq{Owner: smbOwner(a, oa), Open: byID(oa), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); err != nil || !res.Waiting {
		t.Fatalf("a waits: %+v %v", res, err)
	}
	st.fail.Store(true)
	_, uerr := f.Locks().Unlock(ctx, a, file, byID(oa), smbOwner(a, oa), []ByteRange{br(40, 1)}, Seqs{})
	_, lerr := f.lock(a, file, LockReq{Owner: smbOwner(a, oa), Open: byID(oa), Ranges: []LockRange{x(20, 10)}})
	st.fail.Store(false)
	if !errors.Is(uerr, ErrRangeNotLocked) {
		t.Errorf("an unlock of a range not held, over a refusing store: %v, want ErrRangeNotLocked", uerr)
	}
	if !errors.Is(lerr, errWrite) {
		t.Errorf("a lock over a refusing store: %v, want its error", lerr)
	}
	if ws := f.sink.takeWaits(); len(ws) != 0 {
		t.Errorf("the refused lock answered the earlier waiter: %+v", ws)
	}
	f.close(t, b, ob)
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 {
		t.Errorf("granted %v once b closed, want a's earlier waiter", got)
	}
}

// A break that raises a v2 lease's epoch writes it before it is sent, so a
// restart installs the epoch the client last saw; a refused write sends no
// break.
func TestBreakEpochIsWritten(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Epoch, r.Want, r.Parent = rd|wr, LeaseKey{5}, ptr[uint16](3), rwh, LeaseKey{9}
	res, err := f.Opens().Open(ctx, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	if err != nil || res.Grant == nil || res.Grant.Parent != (LeaseKey{9}) {
		t.Fatalf("persistent open: %+v, %v; want a lease with its parent key", res.Grant, err)
	}
	st.fail.Store(true)
	_, err = f.open(b, file, OpenRequest{Access: rd})
	st.fail.Store(false)
	if !errors.Is(err, errWrite) || len(f.sink.takeRecalls()) != 0 {
		t.Errorf("a break over a refusing store: %v, want its error and no recall sent", err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	sent := f.sink.takeRecalls()[0].g.Epoch
	g := f.restart(t)
	back, err := g.reconnect(a, res.Open, ReconnectMatch{Share: file.Share, Create: [16]byte{1}, Lease: LeaseKey{5}}, s2)
	if err != nil || back.Grant == nil || back.Grant.Epoch != sent || back.Grant.Parent != (LeaseKey{9}) {
		t.Errorf("reconnected under %+v, %v; want epoch %d, as the break sent, and the parent key", back.Grant, err, sent)
	}
}

// A lease installed at a client's budget is counted, and the client is
// asked to return grants, as any grant reaching it.
func TestInstalledLeaseCountsInTheBudget(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Want = rd, LeaseKey{5}, rh
	f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	f.Stop()
	g := unstarted(t, f.fs, f.clock)
	if err := g.Start(ctx, g.sink, g.order.order); err != nil {
		t.Fatal(err)
	}
	g.granted[a.Client] = maxGrants - 1
	if err := g.Recovery().ServeShard(ctx, file.Share, file.Shard, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	if n, full := g.granted[a.Client], g.sink.takeRecallAny(); n != maxGrants || len(full) != 1 || full[0].c != a.Client {
		t.Errorf("after the install: %d grants, RecallAny %+v; want the budget reached and the client asked", n, full)
	}
}

// Delete pending set through a persistent open is written, and a restart
// installs it; a refused write sets nothing.
func TestDeletePendingOfPersistentOpenSurvives(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd | del
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	d := true
	st.fail.Store(true)
	err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Delete: &d})
	st.fail.Store(false)
	if !errors.Is(err, errWrite) {
		t.Errorf("delete pending over a refusing store: %v, want its error", err)
	}
	if f.entryOf(file.ID).pending != nil {
		t.Error("the refused delete pending was set")
	}
	if err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Delete: &d}); err != nil {
		t.Fatal(err)
	}
	g := f.restart(t)
	back, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2)
	if err != nil {
		t.Fatal(err)
	}
	if res := g.close(t, a, back.Open); res.Unlink == nil || string(res.Unlink.Name) != "doc" {
		t.Errorf("the last close after the restart: %+v, want the delete pending's unlink of doc", res)
	}

	other, r2 := g.fileIn(t, "kept")
	r2.Access = rd | del
	o2 := g.mustOpen(t, a, other, keep(r2, entity.DurabilityPersistent, s3, time.Minute, 2))
	no := false
	for _, set := range []*bool{&d, &no} {
		if err := g.Opens().Update(ctx, a, o2.ID, OpenUpdate{Delete: set}); err != nil {
			t.Fatal(err)
		}
	}
	h := g.restart(t)
	back2, err := h.reconnect(a, o2, ReconnectMatch{Share: other.Share, Create: [16]byte{2}}, s2)
	if err != nil {
		t.Fatal(err)
	}
	if res := h.close(t, a, back2.Open); res.Unlink != nil {
		t.Errorf("the last close after a cleared delete pending and a restart: %+v, want no unlink", res)
	}
}

// A reconnect before its shard is begun, as while it is installed, is
// ErrDelay.
func TestReconnectWaitsForItsShard(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	g := f.restart(t)
	g.cmu.Lock()
	sh := g.shards[file.Shard]
	delete(g.shards, file.Shard)
	g.cmu.Unlock()
	m := ReconnectMatch{Share: file.Share, Create: [16]byte{1}}
	if _, err := g.reconnect(a, o, m, s2); !errors.Is(err, ErrDelay) {
		t.Errorf("a reconnect in a shard not begun: %v, want ErrDelay", err)
	}
	g.cmu.Lock()
	g.shards[file.Shard] = sh
	g.cmu.Unlock()
	if _, err := g.reconnect(a, o, m, s2); err != nil {
		t.Errorf("the reconnect once its shard is begun: %v", err)
	}
}

// A client install made known is forgotten once it holds nothing, unless it
// came back, by connecting or renewing, which makes it its own again.
func TestInstalledClientIsForgottenUnlessItCameBack(t *testing.T) {
	f := newFixture(t)
	gone, back, renewed := f.caller(t, smbMachine("pc-gone")), smbMachine("pc-back"), f.caller(t, smbMachine("pc-renew"))
	cb := f.caller(t, back)
	for i, who := range []Caller{gone, gone, cb, renewed} {
		file, r := f.fileIn(t, string(rune('a'+i)))
		r.Access = rd
		f.mustOpen(t, who, file, keep(r, entity.DurabilityPersistent, s1, time.Duration(i+1)*time.Minute, byte(i)))
	}
	g := f.restart(t)
	g.connect(t, back)
	g.renew(t, renewed)
	g.idle(time.Minute)
	if _, known := g.known[gone.Client]; !known {
		t.Error("the client was forgotten while an open installed for it is held")
	}
	g.idle(3 * time.Minute)
	if _, known := g.known[gone.Client]; known {
		t.Error("the client that never came back is known once it holds nothing")
	}
	for name, c := range map[string]entity.ClientID{"connected": cb.Client, "renewed": renewed.Client} {
		if _, known := g.known[c]; !known {
			t.Errorf("the client that %s is forgotten once its installed open closed", name)
		}
	}
}

// The records an unlink writes carry the lease, even when the table's
// write after the commit is refused.
func TestUnlinkWritesTheLease(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Want = rd, LeaseKey{5}, rh
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	recs, commit, err := f.Opens().Durable(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.fs.Store.Namespace().Unlink(ctx, f.fs.Admin, f.fs.Root, []byte("doc"), recs...)
	if err != nil {
		t.Fatal(err)
	}
	st.fail.Store(true)
	commit(u.File == file.ID)
	st.fail.Store(false)
	g := f.restart(t)
	back, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}, Lease: LeaseKey{5}}, s2)
	if err != nil || back.Grant == nil || back.Grant.Kind != rh {
		t.Errorf("reconnected under %+v, %v; want the lease at rh", back.Grant, err)
	}
}
