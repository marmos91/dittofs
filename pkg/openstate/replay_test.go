package openstate

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// A replayed create naming an open of its client by its create ID answers
// that open, making none: Replayed, the same open and its lease. Another
// client's replay under the same ID, or one with no ID, is a create of its
// own, and another principal's replay is refused. A create that is no
// replay, a replay of a disconnected open, or one on another session than
// the open's, naming the ID is ErrCreateInUse: one create ID never names two
// opens of a client.
func TestCreateReplay(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file := anyFile()
	r := keep(OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh}, entity.DurabilityDurable, s1, time.Minute, 7)
	first, err := f.Opens().Open(ctx, a, file, r)
	if err != nil || first.Grant == nil {
		t.Fatalf("Open %+v, %v; want it leased", first, err)
	}
	if _, err := f.Opens().Open(ctx, a, file, r); !errors.Is(err, ErrCreateInUse) {
		t.Errorf("a second create under the ID: %v, want ErrCreateInUse", err)
	}
	r.Replay = true
	again, err := f.Opens().Open(ctx, a, file, r)
	if err != nil || !again.Replayed || again.Open.ID != first.Open.ID || again.Grant == nil || again.Grant.ID != first.Grant.ID {
		t.Errorf("the replay %+v, %v; want the first open and its lease, Replayed", again, err)
	}
	if _, err := f.Opens().Open(ctx, Caller{Client: a.Client, Principal: entity.NewUser(), Protocol: a.Protocol}, file, r); !errors.Is(err, ErrNotYours) {
		t.Errorf("another principal's replay: %v, want ErrNotYours", err)
	}
	r.Session = s2
	if res, err := f.Opens().Open(ctx, a, file, r); !errors.Is(err, ErrCreateInUse) {
		t.Errorf("a replay on another session than the open's: %+v, %v; want ErrCreateInUse", res, err)
	}
	r.Session = s1
	r.Lease = LeaseKey{}
	if res, err := f.Opens().Open(ctx, b, file, r); err != nil || res.Replayed {
		t.Errorf("another client's replay under the ID: %+v, %v; want an open of its own", res, err)
	}
	if n := len(f.entryOf(file.ID).opens); n != 2 {
		t.Errorf("%d opens, want the first and the other client's", n)
	}

	// An open named by no create ID is no replay's.
	v1 := keep(OpenRequest{Access: rd, Lease: LeaseKey{2}, Want: rh}, entity.DurabilityDurable, s1, time.Minute, 0)
	f.mustOpen(t, a, file, v1)
	v1.Replay = true
	if res, err := f.Opens().Open(ctx, a, file, v1); err != nil || res.Replayed {
		t.Errorf("a replay naming no create ID: %+v, %v; want an open of its own", res, err)
	}
	f.disconnect(t, a, s1)
	r.Lease, r.Session = LeaseKey{1}, s2
	if _, err := f.Opens().Open(ctx, a, file, r); !errors.Is(err, ErrCreateInUse) {
		t.Errorf("a replay of a disconnected open: %v, want ErrCreateInUse", err)
	}
	if n := len(f.entryOf(file.ID).opens); n != 4 {
		t.Errorf("%d opens, want no create to have made a second open under one ID", n)
	}
}

// A replay creates no new state, so a replayed create is answered in grace
// as a replayed lock is.
func TestCreateReplayInGrace(t *testing.T) {
	f := newFixture(t)
	a, c := f.caller(t, smbMachine("pc-a")), f.caller(t, nfsAs("c", entity.RecoverStatus))
	file, r := f.fileIn(t, "disk")
	r.Access = rd
	q := keep(r, entity.DurabilityPersistent, s1, time.Hour, 1)
	o := f.mustOpen(t, a, file, q)
	f.mustOpen(t, c, file, OpenRequest{Access: rd}) // c holds grace open after the restart
	g := f.restart(t)
	g.renew(t, c)
	if _, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	q.Replay, q.Session = true, s2
	if res, err := g.Opens().Open(ctx, a, file, q); err != nil || !res.Replayed || res.Open.ID != o.ID {
		t.Errorf("a replayed create in grace: %+v, %v; want the reconnected open, Replayed", res, err)
	}
}

// A lock naming its open's lock sequence is stored there once granted, for
// every range of it at once: its retry is answered and not applied, after a
// failover too, while a retry after a refusal is applied.
func TestLockReplay(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd})
	lockAs := func(g *fixture, seq uint8, rs ...LockRange) error {
		_, err := g.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: rs, Replay: &LockSequence{Index: 3, Seq: seq}})
		return err
	}
	if err := lockAs(f, 1, x(0, 10), x(20, 10)); err != nil {
		t.Fatal(err)
	}
	if err := lockAs(f, 1, x(0, 10), x(20, 10)); err != nil {
		t.Errorf("a replay of the lock: %v, want it answered", err)
	}
	f.mustLock(t, b, file, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{x(100, 10)}})
	if err := lockAs(f, 2, x(100, 10)); lockedBy(err) == "" {
		t.Fatalf("a conflicting lock: %v", err)
	}
	if _, err := f.Locks().Unlock(ctx, b, file, byID(ob), smbOwner(b, ob), []ByteRange{br(100, 10)}, Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := lockAs(f, 2, x(100, 10)); err != nil {
		t.Fatalf("the retry after a refusal: %v", err)
	}
	if _, err := f.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{sh(200, 1)}, Replay: &LockSequence{Index: 0, Seq: 1}}); err != nil {
		t.Errorf("a lock naming no index: %v", err)
	}
	if err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err) // a later write of the open's record keeps its lock sequences
	}
	if h := f.holder(t, file, br(100, 10)); h == nil || h.Owner.Client != a.Client {
		t.Errorf("the retry after a refusal holds %+v, want it applied", h)
	}

	g := f.restart(t)
	if _, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	if err := lockAs(g, 2, x(100, 10)); err != nil {
		t.Errorf("a replay after a failover: %v, want it answered", err)
	}
	if got := g.storedLocks(t, file, o); len(got) != 4 {
		t.Errorf("stored %+v, want each lock once", got)
	}

	// An unlock under a lock sequence is answered on its replay, not refused
	// for the range it already released, after a failover too.
	unlockAs := func(g *fixture) error {
		_, err := g.Locks().Unlock(ctx, a, file, byID(o), smbOwner(a, o), []ByteRange{br(0, 10)}, Seqs{}, &LockSequence{Index: 4, Seq: 1})
		return err
	}
	if err := unlockAs(g); err != nil {
		t.Fatal(err)
	}
	if err := unlockAs(g); err != nil {
		t.Errorf("a replay of the unlock: %v, want it answered", err)
	}
	h := g.restart(t)
	if _, err := h.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s3); err != nil {
		t.Fatal(err)
	}
	if err := unlockAs(h); err != nil {
		t.Errorf("a replay of the unlock after a failover: %v, want it answered", err)
	}
	// One stopped at a range not held stores nothing: its retry is applied.
	h.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(300, 5)}})
	partial := func() error {
		_, err := h.Locks().Unlock(ctx, a, file, byID(o), smbOwner(a, o), []ByteRange{br(300, 5), br(500, 5)}, Seqs{}, &LockSequence{Index: 5, Seq: 1})
		return err
	}
	if err := partial(); !errors.Is(err, ErrRangeNotLocked) {
		t.Fatalf("an unlock of a range not held: %v", err)
	}
	if err := partial(); !errors.Is(err, ErrRangeNotLocked) {
		t.Errorf("the retry of an unlock stopped at a range not held: %v, want it applied", err)
	}
}

// A create naming an app instance another client's open of the file holds
// closes that open first, a persistent one installed by a restart included,
// unless the caller may not read the file, the open is the caller's own
// client's, or both are versioned and the create's version is not higher:
// then the create meets the open's deny mode as any other, or, for the
// version, is refused.
func TestAppInstanceReplacesItsPredecessor(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Deny = rd|wr, wr
	as := func(who Caller, version uint64, read bool) OpenRequest {
		q := keep(r, entity.DurabilityPersistent, s1, time.Hour, byte(who.Client))
		q.Durable.Instance, q.MaxRead = AppInstance{ID: [16]byte{9}, Version: [2]uint64{1, version}}, read
		return q
	}
	oa := f.mustOpen(t, a, file, as(a, 5, true))
	for what, q := range map[string]struct {
		who Caller
		r   OpenRequest
	}{
		"a caller that may not read":   {b, as(b, 6, false)},
		"the open's own client":        {a, func() OpenRequest { q := as(a, 6, true); q.Durable.Create[1] = 1; return q }()},
		"a version not above the open": {b, as(b, 5, true)},
	} {
		_, err := f.open(q.who, file, q.r)
		if !errors.Is(err, ErrShareViolation) || errors.Is(err, ErrInstanceVersion) != (what == "a version not above the open") {
			t.Errorf("%s: %v, want ErrShareViolation, ErrInstanceVersion only for the version", what, err)
		}
	}
	if n := len(f.entryOf(file.ID).opens); n != 1 {
		t.Fatalf("%d opens, want only the first", n)
	}

	g := f.restart(t)
	g.renew(t, b)
	g.idle(leasePeriod) // past grace, inside the installed open's Timeout
	ob := g.mustOpen(t, b, file, as(b, 6, true))
	if got := g.stored(t, file); len(got) != 1 || got[0].ID != ob.ID {
		t.Errorf("stored %+v, want only the open that replaced the installed one", got)
	}
	if _, err := g.reconnect(a, oa, ReconnectMatch{Share: file.Share, Create: [16]byte{byte(a.Client)}}, s2); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect of the replaced open: %v, want ErrNoReclaim", err)
	}
}

// A request through an open is judged by its channel sequence: an older one
// is stale, a newer one raises the open's, written before it runs for a
// persistent open so a failover keeps it, and a replay is stale while a
// request under an earlier sequence is in flight.
func TestChannelSequence(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Channel = rd|wr, 5
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	ch := func(g *fixture, seq uint16, replay bool) (bool, func()) {
		t.Helper()
		stale, done, err := g.Opens().Channel(ctx, a, o.ID, seq, replay)
		if err != nil {
			t.Fatalf("Channel %d: %v", seq, err)
		}
		return stale, done
	}
	if stale, _ := ch(f, 4, false); !stale {
		t.Error("a request on a sequence older than the create's is not stale")
	}
	stale, first := ch(f, 5, false)
	if stale {
		t.Fatal("a request on the open's own sequence is stale")
	}
	if stale, _ := ch(f, 6, false); stale {
		t.Error("a request on a newer sequence is stale")
	}
	if stale, _ := ch(f, 6, true); !stale {
		t.Error("a replay while a request under an earlier sequence is in flight is not stale")
	}
	first()
	if stale, _ := ch(f, 6, true); stale {
		t.Error("a replay once the earlier request left is stale")
	}
	if stale, _ := ch(f, 6+0x8000, false); !stale {
		t.Error("a sequence half the space away is not older")
	}
	if err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err) // a later write of the open's record keeps its sequence
	}

	g := f.restart(t)
	if _, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	if stale, _ := ch(g, 5, false); !stale {
		t.Error("after a failover, the sequence a request raised is lost")
	}
}

// A blocking lock granted by callback stores its lock sequence, written
// with the lock for a persistent open, so its replay after a lost reply is
// answered rather than waiting on its own lock, after a failover too.
func TestBlockingLockReplay(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd | wr})
	f.mustLock(t, b, file, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{x(0, 10)}})
	req := LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback, Replay: &LockSequence{Index: 1, Seq: 1}}
	if res, err := f.lock(a, file, req); err != nil || !res.Waiting {
		t.Fatalf("the waiter: %+v, %v", res, err)
	}
	if _, err := f.Locks().Unlock(ctx, b, file, byID(ob), smbOwner(b, ob), []ByteRange{br(0, 10)}, Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
	if w := f.sink.takeWaits(); len(w) != 1 || !w[0].granted {
		t.Fatalf("waits %+v, want the waiter granted", w)
	}
	if res, err := f.lock(a, file, req); err != nil || res.Waiting {
		t.Errorf("a replay of a callback-granted lock: %+v, %v; want it answered", res, err)
	}
	g := f.restart(t)
	if _, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	if res, err := g.lock(a, file, req); err != nil || res.Waiting {
		t.Errorf("a replay of a callback-granted lock after a failover: %+v, %v; want it answered", res, err)
	}
}

// A request that leaves while its open is disconnected is uncounted all the
// same, and a count is never taken below zero, so once every request has
// left a replay on a new channel runs.
func TestChannelLeaveIsAlwaysCounted(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Channel = rd|wr, 5
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	channel := func(seq uint16, replay bool) (bool, func()) {
		t.Helper()
		stale, done, err := f.Opens().Channel(ctx, a, o.ID, seq, replay)
		if err != nil {
			t.Fatalf("Channel %d: %v", seq, err)
		}
		return stale, done
	}
	_, done := channel(5, false)
	f.disconnect(t, a, s1)
	done() // a write in flight finishes after its connection dropped
	if _, err := f.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	stale, done := channel(6, true)
	if stale {
		t.Error("a replay once a request that left while disconnected is gone: stale")
	}
	done()
	_, done = channel(6, false)
	done()
	done() // told twice
	if stale, _ := channel(7, true); stale {
		t.Error("a replay after a request told it left twice: stale")
	}
}

// A lock or an unlock under another sequence than its index stores is no
// replay: the index stops naming the stored one, so the 4-bit sequence
// wrapping round to it later is applied, not answered. A persistent open's
// record follows.
func TestLockSequenceMismatchInvalidates(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd})
	lockAs := func(seq uint8, rg LockRange) error {
		_, err := f.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{rg}, Replay: &LockSequence{Index: 3, Seq: seq}})
		return err
	}
	if err := lockAs(1, x(0, 10)); err != nil {
		t.Fatal(err)
	}
	f.mustLock(t, b, file, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{x(100, 10)}})
	for seq := uint8(2); seq != 17; seq++ {
		if err := lockAs(seq&0xF, x(100, 10)); !errors.Is(err, ErrLocked) {
			t.Fatalf("sequence %d of a conflicting lock: %v, want the conflict", seq&0xF, err)
		}
	}
	if err := lockAs(1, x(100, 10)); !errors.Is(err, ErrLocked) {
		t.Errorf("a new lock whose index wrapped to a stale sequence: %v; want the conflict, not a replay's success", err)
	}
	f.step(file.ID, func(e *entry) error {
		f.timeouts(ctx, e, &outbox{}) // the timer writes the open's record again
		return nil
	})
	if got := f.stored(t, file); len(got) != 1 || got[0].LockSeq[2] != 0 {
		t.Errorf("stored %+v, want the reset index written", got)
	}

	unlockAs := func(seq uint8, rg ByteRange) error {
		_, err := f.Locks().Unlock(ctx, a, file, byID(o), smbOwner(a, o), []ByteRange{rg}, Seqs{}, &LockSequence{Index: 4, Seq: seq})
		return err
	}
	if err := unlockAs(1, br(0, 10)); err != nil {
		t.Fatal(err)
	}
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}})
	if err := unlockAs(2, br(500, 5)); !errors.Is(err, ErrRangeNotLocked) {
		t.Fatalf("an unlock of a range not held: %v", err)
	}
	if err := unlockAs(1, br(0, 10)); err != nil {
		t.Fatal(err)
	}
	if h := f.holder(t, file, br(0, 10)); h != nil {
		t.Errorf("an unlock whose index another sequence reset was answered as a replay: %+v still held", h)
	}
}

// An app-instance create that is its client's first state in the shard,
// refused where the store refuses its client's shard mark, has closed no
// predecessor.
func TestAppInstanceWaitsForItsShardMark(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file := in(1)
	as := func(who Caller, version uint64) OpenRequest {
		q := keep(OpenRequest{Access: rd}, entity.DurabilityDurable, s1, time.Hour, byte(who.Client))
		q.Durable.Instance, q.MaxRead = AppInstance{ID: [16]byte{9}, Version: [2]uint64{1, version}}, true
		return q
	}
	f.mustOpen(t, a, file, as(a, 5))
	st.updateAt.Store(st.updates.Load() + 1)
	if _, err := f.open(b, file, as(b, 6)); !errors.Is(err, errWrite) {
		t.Fatalf("a create whose shard mark the store refuses: %v, want the store's error", err)
	}
	if e := f.entryOf(file.ID); e == nil || len(e.opens) != 1 {
		t.Error("a create refused at its shard mark closed its predecessor")
	}
	if _, err := f.open(b, file, as(b, 6)); err != nil {
		t.Fatalf("the create once the store writes again: %v", err)
	}
	if n := len(f.entryOf(file.ID).opens); n != 1 {
		t.Errorf("%d opens, want only the one that replaced its predecessor", n)
	}
}

// A channel sequence exactly half the space from the open's is stale on
// either side of it, a request wrapping round to it included: the
// difference is taken unsigned in 16 bits, and only one up to 0x7FFF is a
// step forward.
func TestChannelSequenceHalfSpaceIsStale(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	for _, c := range []struct{ open, req uint16 }{{5, 0x8005}, {0x8005, 5}, {0xFFFF, 0x7FFF}} {
		r := keep(OpenRequest{Access: rd, Channel: c.open}, entity.DurabilityDurable, s1, time.Hour, byte(c.open))
		o := f.mustOpen(t, a, anyFile(), r)
		stale, _, err := f.Opens().Channel(ctx, a, o.ID, c.req, false)
		if err != nil || !stale {
			t.Errorf("a request on %#x through an open on %#x: stale %v, %v; want stale", c.req, c.open, stale, err)
		}
		if stale, _, _ := f.Opens().Channel(ctx, a, o.ID, c.open+0x7FFF, false); stale {
			t.Errorf("a request on %#x through an open on %#x: stale, want a step forward", c.open+0x7FFF, c.open)
		}
	}
}
