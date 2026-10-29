package openstate

import (
	"errors"
	"testing"
	"testing/iotest"
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

// A lock or an unlock under a persistent open holding many locks writes
// only the locks it changes, in one store transaction, and a restart holds
// the locks as changed.
func TestPersistentLockChangeIsOneWrite(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, 300*time.Second, 1))
	lock := func(off uint64) {
		f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(off, 10)}})
	}
	for i := range 64 {
		lock(uint64(i) * 20)
	}
	for _, c := range []struct {
		name string
		do   func()
	}{
		{"a lock", func() { lock(5000) }},
		{"an unlock among them", func() {
			if _, err := f.Locks().Unlock(ctx, a, file, byID(o), smbOwner(a, o), []ByteRange{br(200, 10)}, Seqs{}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		st.updates.Store(0)
		fs.Counts.Take()
		c.do()
		if n, w := st.updates.Load(), fs.Counts.Take(); n != 1 || w.Sets+w.Deletes != 1 {
			t.Errorf("%s under 64 locks: %d transactions writing %d keys, want 1 writing 1", c.name, n, w.Sets+w.Deletes)
		}
	}
	g := f.restart(t)
	g.renew(t, b)
	g.idle(120 * time.Second)
	for off, want := range map[uint64]error{5005: ErrLocked, 205: nil, 225: ErrLocked} {
		if err := g.anon(file, off); !errors.Is(err, want) {
			t.Errorf("an anonymous read at %d after a restart: %v, want %v", off, err, want)
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

// Once a shard's grace has ended, the sweep closes every durable record of
// it that nobody reclaimed and that is not persistent: an unlinked file's
// open lost with its shard, and a record a failed delete left, go, each file
// left with no holder raised an orphan once. A reclaimed open's record, and a
// persistent one, stay.
func TestGraceEndClosesUnreclaimedRecords(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, nfsAs("a", entity.RecoverStatus))
	gone := f.caller(t, nfsAs("gone", entity.RecoverStatus))
	p := f.caller(t, smbMachine("pc-p"))
	lost, lr := f.fileIn(t, "lost")
	kept, kr := f.fileIn(t, "kept")
	late, tr := f.fileIn(t, "late")
	left, fr := f.fileIn(t, "left")
	disk, dr := f.fileIn(t, "disk")
	lr.Access, kr.Access, fr.Access, dr.Access, tr.Access = rd, rd, rd, rd, rd
	kr.Owner, tr.Owner = owner(a, "oo"), owner(a, "oo")
	f.mustOpen(t, a, late, tr)
	f.mustOpen(t, a, lost, lr)
	f.mustOpen(t, a, kept, kr)
	f.mustOpen(t, gone, left, fr)
	f.mustOpen(t, p, disk, keep(dr, entity.DurabilityPersistent, s1, time.Hour, 1))
	for name, file := range map[string]FileKey{"lost": lost, "kept": kept, "left": left, "disk": disk} {
		f.unlink(t, file, name)
	}
	st.fail.Store(true)
	if err := f.Expire(ctx, gone.Client); err != nil {
		t.Fatal(err)
	}
	st.fail.Store(false)
	if got := f.stored(t, left); len(got) != 1 {
		t.Fatalf("stored after a failed delete: %+v, want its record left", got)
	}

	g := f.restart(t)
	g.renew(t, a)
	kr.Reclaim, kr.Unlinked = true, true
	g.mustOpen(t, a, kept, kr)
	tr.Reclaim = true
	g.mustOpen(t, a, late, tr)
	g.unlink(t, late, "late") // in grace, so its record is written by this instance
	g.ordered(lost)
	g.closeUnreclaimed(ctx, g.sink)
	if got := g.stored(t, lost); len(got) != 1 {
		t.Errorf("stored in grace: %+v, want the record kept for its reclaim", got)
	}
	if g.ordered(lost) {
		t.Error("the close in grace took a file's admission lock")
	}
	for _, e := range g.sink.graces {
		if e.s == lost.Shard {
			t.Errorf("GraceEnded for shard %d in its grace", e.s)
		}
	}
	g.pass(t, leasePeriod, a.Client)
	st.fail.Store(true)
	g.closeUnreclaimed(ctx, g.sink) // refused: the next sweep runs it again
	st.fail.Store(false)
	for _, e := range g.sink.graces {
		if e.s == lost.Shard {
			t.Errorf("GraceEnded for shard %d after a refused close", e.s)
		}
	}
	g.sweep(ctx)
	g.Stop() // waits for the steps the sweep started
	if !g.ordered(lost) {
		t.Error("the grace end's close ran without the file's admission lock")
	}
	st.reads.Store(0)
	st.readAt.Store(1 << 40) // counts reads
	g.closeUnreclaimed(ctx, g.sink)
	if n := st.reads.Load(); n != 0 {
		t.Errorf("a pass after the shard's records were closed read the store %d times, want none", n)
	}
	st.readAt.Store(0)
	orphans := map[entity.FileID]int{}
	for _, c := range g.sink.closed {
		if c.r.Orphan {
			orphans[c.f]++
		}
	}
	// Each shard's grace ends once, after the Closed of every file its close left.
	last := map[ShardID]int{}
	for i, c := range g.sink.closed {
		for _, file := range []FileKey{lost, left} {
			if c.r.Orphan && c.f == file.ID {
				last[file.Shard] = i + 1
			}
		}
	}
	ends := map[ShardID]int{}
	for _, e := range g.sink.graces {
		ends[e.s]++
		if e.share != fs.Share.ID || e.closed < last[e.s] {
			t.Errorf("GraceEnded %+v: want share %v, after the %d Closed its close raised", e, fs.Share.ID, last[e.s])
		}
	}
	for _, s := range testShards {
		if ends[s] != 1 {
			t.Errorf("shard %d's grace ended %d times, want once", s, ends[s])
		}
	}
	for name, file := range map[string]FileKey{"lost": lost, "left": left} {
		if got := g.stored(t, file); len(got) != 0 || orphans[file.ID] != 1 {
			t.Errorf("%s after grace: stored %+v, raised an orphan %d times; want no record and one orphan", name, got, orphans[file.ID])
		}
	}
	for name, file := range map[string]FileKey{"kept": kept, "late": late, "disk": disk} {
		if got := g.stored(t, file); len(got) != 1 || orphans[file.ID] != 0 {
			t.Errorf("%s after grace: stored %+v, raised an orphan %d times; want its one record kept", name, got, orphans[file.ID])
		}
	}
}

// A disconnected durable open whose lease's recall times out holds handle
// caching no longer, so it closes, and its deny mode with it.
func TestLapsedRecallClosesAParkedDurableOpen(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	f.mustOpen(t, a, file, keep(OpenRequest{Access: rd, Deny: wr, Lease: LeaseKey{7}, Want: rwh}, entity.DurabilityDurable, s1, time.Hour, 1))
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatalf("b's read open: %v, want the lease recalled", err)
	}
	f.disconnect(t, a, s1)
	f.pass(t, coversDeadline+time.Second)
	f.idle(0)
	if _, err := f.open(b, file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open once the parked open's recall lapsed: %v", err)
	}
}

// pendingStored reports whether the store holds file delete pending.
func (f *fixture) pendingStored(t *testing.T, file FileKey) bool {
	t.Helper()
	_, ok, err := f.fs.Store.Opens().DeletePending(ctx, f.fs.Share.ID, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A close with delete on close beside another client's persistent open
// writes the delete pending it leaves before it is answered; a refused write
// refuses the close and changes nothing.
func TestClosePendingIsWritten(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd
	f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd | del, DeleteOnClose: true})

	st.fail.Store(true)
	_, err := f.Opens().Close(ctx, b, refOf(ob), Seqs{})
	st.fail.Store(false)
	if !errors.Is(err, errWrite) {
		t.Fatalf("a delete-on-close close over a refusing store: %v, want its error", err)
	}
	if f.entryOf(file.ID).pending != nil || f.pendingStored(t, file) {
		t.Error("the refused close left the file delete pending")
	}
	f.close(t, b, ob)
	if !f.pendingStored(t, file) {
		t.Error("the close left the file delete pending in memory only")
	}
}

// A refused close whose delete pending was written before its record's
// delete failed takes the delete pending back.
func TestRefusedCloseTakesItsPendingBack(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd
	f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd | del, DeleteOnClose: true})
	f.unlink(t, file, "doc")                 // b's open is written durable
	st.updateAt.Store(st.updates.Load() + 2) // the pending's write passes, the record's delete fails
	_, err := f.Opens().Close(ctx, b, refOf(ob), Seqs{})
	st.updateAt.Store(0)
	if !errors.Is(err, errWrite) {
		t.Fatalf("a close whose record's delete is refused: %v, want its error", err)
	}
	if f.pendingStored(t, file) {
		t.Error("the refused close left its delete pending written")
	}
}

// A close by the table with delete on close, whose delete pending's write
// is refused, has it written again by the timer.
func TestTimedOutClosePendingIsWrittenAgain(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd
	f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	f.mustOpen(t, b, file, keep(OpenRequest{Access: rd | del, DeleteOnClose: true, Lease: LeaseKey{3}, Want: rh}, entity.DurabilityDurable, s2, time.Minute, 2))
	f.disconnect(t, b, s2)
	st.fail.Store(true)
	f.idle(time.Minute) // b's Timeout passes
	st.fail.Store(false)
	if f.pendingStored(t, file) {
		t.Fatal("the refused write stored the delete pending")
	}
	f.idle(0)
	if !f.pendingStored(t, file) {
		t.Error("the timer did not write the delete pending again")
	}
}

// An update setting delete pending and a suspended time, whose delete
// pending's write is refused after its record's write passed, puts the
// record back.
func TestRefusedUpdatePutsItsRecordBack(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd | del
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	d := true
	st.updateAt.Store(st.updates.Load() + 2)
	err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Delete: &d, Suspend: entity.TimeModify})
	st.updateAt.Store(0)
	if !errors.Is(err, errWrite) {
		t.Fatalf("an update whose delete pending's write is refused: %v, want its error", err)
	}
	if f.pendingStored(t, file) {
		t.Error("the refused update stored the delete pending")
	}
	if recs := f.stored(t, file); len(recs) != 1 || recs[0].Suspended != 0 {
		t.Errorf("the refused update left its record %+v, want nothing suspended", recs)
	}
}

// storedLocks is the locks the store holds under persistent open o.
func (f *fixture) storedLocks(t *testing.T, file FileKey, o Open) []entity.LockRecord {
	t.Helper()
	got, err := f.fs.Store.Opens().Locks(ctx, f.fs.Share.ID, file.ID, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A lock under a persistent open refused because its lock state cannot be
// minted writes no lock, and one whose write is refused leaves no lock state.
func TestLockRefusedAtItsStateWritesNothing(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	rnd := f.d.Rand
	f.d.Rand = iotest.ErrReader(errWrite) // no reference can be minted
	_, err := f.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}})
	f.d.Rand = rnd
	if !errors.Is(err, errWrite) {
		t.Fatalf("a lock whose lock state cannot be minted: %v, want its error", err)
	}
	if got := f.storedLocks(t, file, o); len(got) != 0 {
		t.Errorf("the refused lock was written: %+v", got)
	}
	st.fail.Store(true)
	_, err = f.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}})
	st.fail.Store(false)
	if !errors.Is(err, errWrite) {
		t.Fatalf("a lock over a refusing store: %v, want its error", err)
	}
	if n := len(f.entryOf(file.ID).lstates); n != 0 {
		t.Errorf("the refused lock left %d lock states", n)
	}
}

// A disconnected open is used by nothing until it is reconnected: a lock, an
// unlock, an update or a close naming it is ErrBadState and changes nothing.
func TestParkedOpenIsNotUsedByItsID(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr | del
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}})
	f.disconnect(t, a, s1)
	d := true
	for name, err := range map[string]error{
		"a lock": func() error {
			_, err := f.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(20, 10)}})
			return err
		}(),
		"an unlock": func() error {
			_, err := f.Locks().Unlock(ctx, a, file, byID(o), smbOwner(a, o), []ByteRange{br(0, 10)}, Seqs{})
			return err
		}(),
		"an update": f.Opens().Update(ctx, a, o.ID, OpenUpdate{Delete: &d, Suspend: entity.TimeModify}),
		"a close":   func() error { _, err := f.Opens().Close(ctx, a, byID(o), Seqs{}); return err }(),
	} {
		if !errors.Is(err, ErrBadState) {
			t.Errorf("%s through the disconnected open: %v, want ErrBadState", name, err)
		}
	}
	if recs := f.stored(t, file); len(recs) != 1 || recs[0].Suspended != 0 {
		t.Errorf("the open's record after the refusals: %+v", recs)
	}
	if got := f.storedLocks(t, file, o); len(got) != 1 {
		t.Errorf("the open's locks after the refusals: %+v", got)
	}
	if _, err := f.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Errorf("the reconnect after the refusals: %v", err)
	}
}

// A grace-end pass that finds its shard served again under a new instance
// stops: it closes none of the records the new grace waits on, and raises no
// GraceEnded inside it.
func TestGraceEndPassStopsWhenServedAgain(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfsAs("a", entity.RecoverStatus))
	lost, lr := f.fileIn(t, "lost")
	lr.Access = rd
	f.mustOpen(t, a, lost, lr)
	f.unlink(t, lost, "lost")
	g := f.restart(t)
	g.pass(t, leasePeriod)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	g.Table.order = func(entity.FileID) func() { entered <- struct{}{}; <-gate; return func() {} }
	done := make(chan struct{})
	go func() { g.closeUnreclaimed(ctx, g.sink); close(done) }()
	<-entered // the pass waits on lost's admission lock
	if err := g.Recovery().ServeShard(ctx, g.fs.Share.ID, lost.Shard, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	close(gate)
	<-done
	if got := g.stored(t, lost); len(got) != 1 {
		t.Errorf("stored after a pass over a shard served again: %+v, want the record kept for the new grace", got)
	}
	for _, e := range g.sink.graces {
		if e.s == lost.Shard {
			t.Errorf("GraceEnded for shard %d inside its new grace", e.s)
		}
	}
}

// A grace-end pass whose shard is served again after its last record raises
// no GraceEnded inside the new grace.
func TestGraceEndIsNotRaisedWhenServedAgain(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfsAs("a", entity.RecoverStatus))
	lost, lr := f.fileIn(t, "lost")
	lr.Access = rd
	f.mustOpen(t, a, lost, lr)
	f.unlink(t, lost, "lost")
	g := f.restart(t)
	g.pass(t, leasePeriod)
	g.Table.order = func(entity.FileID) func() {
		return func() { // after the record's step: the shard is served again
			if err := g.Recovery().ServeShard(ctx, g.fs.Share.ID, lost.Shard, instances.Add(1)); err != nil {
				t.Error(err)
			}
		}
	}
	g.closeUnreclaimed(ctx, g.sink)
	for _, e := range g.sink.graces {
		if e.s == lost.Shard {
			t.Errorf("GraceEnded for shard %d inside its new grace", e.s)
		}
	}
}

// A lock whose stored name another lock takes over is rewritten: of two
// stacked locks at one offset, unlocking the first leaves the second named
// as the first was.
func TestLockRenamedInPlaceIsRewritten(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	so := smbOwner(a, o)
	f.mustLock(t, a, file, LockReq{Owner: so, Open: byID(o), Ranges: []LockRange{sh(0, 10)}})
	f.mustLock(t, a, file, LockReq{Owner: so, Open: byID(o), Ranges: []LockRange{sh(0, 20)}})
	if _, err := f.Locks().Unlock(ctx, a, file, byID(o), so, []ByteRange{br(0, 10)}, Seqs{}); err != nil {
		t.Fatal(err)
	}
	if got := f.storedLocks(t, file, o); len(got) != 1 || got[0].Length != 20 {
		t.Errorf("stored after the first of two stacked locks went: %+v, want the second alone", got)
	}
}

// A break of read caching alone ends as it is sent, and is written as it
// ends, a v1 lease's as a v2's: the stored lease holds no caching, as the
// client was told.
func TestReadOnlyBreakIsWrittenAsItEnds(t *testing.T) {
	for _, epoch := range []*uint16{ptr[uint16](3), nil} {
		f := newFixture(t)
		a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
		file, r := f.fileIn(t, "doc")
		r.Access, r.Lease, r.Epoch, r.Want = rd, LeaseKey{5}, epoch, GrantRead
		f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
		if _, err := f.open(b, file, OpenRequest{Access: wr}); err != nil {
			t.Fatalf("a writer beside an R lease: %v", err)
		}
		sent := f.sink.takeRecalls()
		recs := f.stored(t, file)
		if len(sent) != 1 || len(recs) != 1 {
			t.Fatalf("stored %+v after the break %+v, want one record and one break", recs, sent)
		}
		if l := recs[0].Lease; l == nil || l.State != 0 || l.Epoch != sent[0].g.Epoch {
			t.Errorf("v2 %v: stored lease %+v, want it at none, at the break's epoch %d", epoch != nil, l, sent[0].g.Epoch)
		}
	}
}
