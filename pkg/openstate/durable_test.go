package openstate

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

var (
	s1 = entity.SessionRef{Node: 1, NodeEpoch: 1, ID: 1}
	s2 = entity.SessionRef{Node: 1, NodeEpoch: 1, ID: 2}
	s3 = entity.SessionRef{Node: 1, NodeEpoch: 1, ID: 3}
)

// smbMachine is an SMB client that recovers by reconnecting.
func smbMachine(name string) Client {
	c := smb(name)
	c.Recovery = entity.RecoverReconnect
	return c
}

// keep makes r an open of session s kept as d says for timeout, under the
// create ID create.
func keep(r OpenRequest, d entity.Durability, s entity.SessionRef, timeout time.Duration, create byte) OpenRequest {
	r.Durability, r.Session, r.Durable = d, s, &DurableKeys{Timeout: timeout, Create: [16]byte{create}}
	return r
}

func byID(o Open) OpenRef {
	return OpenRef{Kind: RefOpened, Open: o.ID, State: StateRef{Shard: o.Ref.Shard}}
}

func (f *fixture) disconnect(t *testing.T, who Caller, s entity.SessionRef) {
	t.Helper()
	if err := f.Disconnect(ctx, who.Client, s); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func (f *fixture) reconnect(who Caller, o Open, m ReconnectMatch, s entity.SessionRef) (OpenResult, error) {
	return f.Opens().Reconnect(ctx, who, o.ID, m, s)
}

// idle moves the clock d, firing no sweep, then runs what the timer runs on
// each file holding a disconnected open.
func (f *fixture) idle(d time.Duration) {
	f.clock.mu.Lock()
	f.clock.now += d
	f.clock.mu.Unlock()
	f.idx.Lock()
	files := slices.Collect(maps.Keys(f.parked))
	f.idx.Unlock()
	f.serveFiles(ctx, f.sink, files)
}

// A disconnect ends one session: its volatile opens close, serving the
// waiters they held under the file's admission lock, and its durable opens
// stay with their deny modes. Another session of the machine, and a session
// of the same ID on another node, keep their opens.
func TestDisconnectEndsOneSession(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	volatile, durable, other, elsewhere := anyFile(), anyFile(), anyFile(), anyFile()
	deny := OpenRequest{Access: rd | wr, Deny: wr}
	va := f.mustOpen(t, a, volatile, keep(deny, entity.DurabilityNone, s1, 0, 0))
	va.Durable = nil
	f.mustOpen(t, a, durable, keep(deny, entity.DurabilityDurable, s1, time.Minute, 1))
	f.mustOpen(t, a, other, keep(deny, entity.DurabilityDurable, s2, time.Minute, 2))
	f.mustOpen(t, a, elsewhere, keep(deny, entity.DurabilityDurable, entity.SessionRef{Node: 2, NodeEpoch: 1, ID: 1}, time.Minute, 3))
	f.mustLock(t, a, volatile, LockReq{Owner: smbOwner(a, va), Open: byID(va), Ranges: []LockRange{x(0, 10)}})
	ob := f.mustOpen(t, b, volatile, OpenRequest{Access: rd})
	if res, err := f.lock(b, volatile, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); err != nil || !res.Waiting {
		t.Fatalf("b's lock behind a's: %+v, %v; want waiting", res, err)
	}
	f.ordered(volatile)

	f.disconnect(t, a, s1)
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 {
		t.Errorf("waiters granted by the disconnect: %v, want b's", got)
	}
	if !f.ordered(volatile) {
		t.Error("the waiter the disconnect freed was served without the file's admission lock")
	}
	if _, err := f.open(b, volatile, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open after the disconnect closed the volatile open: %v", err)
	}
	for name, file := range map[string]FileKey{"the durable open": durable, "another session's": other, "another node's session's": elsewhere} {
		if _, err := f.open(b, file, OpenRequest{Access: wr}); !errors.Is(err, ErrShareViolation) {
			t.Errorf("a write open against %s deny-write: %v, want ErrShareViolation", name, err)
		}
	}
}

// A disconnected durable open is closed once its Timeout passes without a
// reconnect, by the timer, under the file's admission lock, as a close: a
// delete on close it carries is raised. Until then a reconnect finds it, and
// after its Timeout none does, the timer not yet run.
func TestDurableOpenTimesOut(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Deny, r.DeleteOnClose = rd|wr|del, wr, true
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityDurable, s1, time.Minute, 1))
	f.disconnect(t, a, s1)
	f.idle(30 * time.Second)
	f.disconnect(t, a, s1) // the session's end, told again, restarts nothing

	f.idle(30*time.Second - time.Second)
	if _, err := f.open(b, file, OpenRequest{Access: wr}); !errors.Is(err, ErrShareViolation) {
		t.Errorf("a write open inside the Timeout: %v, want ErrShareViolation", err)
	}
	if len(f.sink.closed) != 0 {
		t.Errorf("closed inside its Timeout: %+v", f.sink.closed)
	}
	f.clock.mu.Lock()
	f.clock.now += 2 * time.Second
	f.clock.mu.Unlock()
	if _, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s2); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect past the Timeout: %v, want ErrNoReclaim", err)
	}
	f.ordered(file)
	f.sweep(ctx)
	f.Stop() // waits for the steps the sweep started
	if len(f.sink.closed) != 1 || f.sink.closed[0].f != file.ID || f.sink.closed[0].r.Unlink == nil {
		t.Errorf("Closed past the Timeout: %+v, want the deferred unlink", f.sink.closed)
	}
	if !f.ordered(file) {
		t.Error("the Timeout's close ran without the file's admission lock")
	}
}

// A reconnect matches only the open's client, principal, create ID and lease
// key, and attaches the open to the reconnecting session: a second reconnect
// finds it attached, a disconnect of the session it left starts nothing, and
// one of the session it joined starts its Timeout again.
func TestReconnectMatches(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	k := LeaseKey{7}
	req := keep(OpenRequest{Access: rd, Lease: k, Epoch: ptr[uint16](1), Want: rh}, entity.DurabilityDurable, s1, time.Minute, 9)
	o, g := f.grantOpen(t, a, file, req)
	if g == nil {
		t.Fatal("no lease offered")
	}
	exact := ReconnectMatch{Create: [16]byte{9}, Lease: k}
	*req.Durable = DurableKeys{} // the caller's keys are its own: the open keeps a copy
	if _, err := f.reconnect(a, o, exact, s2); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect of an open still attached: %v, want ErrNoReclaim", err)
	}
	f.disconnect(t, a, s1)
	for name, try := range map[string]func() error{
		"another client": func() error {
			_, err := f.reconnect(Caller{b.Client, a.Principal, entity.ProtocolSMB}, o, exact, s2)
			return err
		},
		"another principal": func() error {
			_, err := f.reconnect(Caller{a.Client, entity.NewUser(), entity.ProtocolSMB}, o, exact, s2)
			return err
		},
		"another create ID": func() error {
			_, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{8}, Lease: k}, s2)
			return err
		},
		"another lease key": func() error {
			_, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{9}, Lease: LeaseKey{6}}, s2)
			return err
		},
		"no lease key": func() error { _, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{9}}, s2); return err },
		"no open":      func() error { _, err := f.Opens().Reconnect(ctx, a, o.ID+1000, exact, s2); return err },
	} {
		if err := try(); !errors.Is(err, ErrNoReclaim) {
			t.Errorf("a reconnect with %s: %v, want ErrNoReclaim", name, err)
		}
	}
	res, err := f.reconnect(a, o, exact, s2)
	if err != nil {
		t.Fatalf("the exact reconnect: %v", err)
	}
	if res.Open.Session != s2 || res.Grant == nil || res.Grant.ID != g.ID || res.Grant.Kind != rh {
		t.Errorf("reconnected %+v with %+v; want session s2 and its lease", res.Open, res.Grant)
	}
	if _, err := f.reconnect(a, o, exact, s3); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a second reconnect: %v, want ErrNoReclaim", err)
	}
	f.disconnect(t, a, s1)
	if _, err := f.reconnect(a, o, exact, s3); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect after the session it left ended: %v, want ErrNoReclaim", err)
	}
	f.disconnect(t, a, s2)
	if _, err := f.reconnect(a, o, exact, s3); err != nil {
		t.Errorf("a reconnect after the session it joined ended: %v", err)
	}
}

// A durable open lives only in its primary's table: after a restart a
// reconnect finds no open.
func TestDurableOpenDoesNotSurviveARestart(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityDurable, s1, time.Minute, 1))
	f.disconnect(t, a, s1)
	g := f.restart(t)
	if _, err := g.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s2); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect of a durable open after a restart: %v, want ErrNoReclaim", err)
	}
}

// A persistent open is written at open, with its lease in SMB's numbering,
// and gone at close. Only an SMB open, with its keys, is kept.
func TestPersistentOpenIsWritten(t *testing.T) {
	f := newFixture(t)
	a, n := f.caller(t, smb("pc-a")), f.caller(t, nfs("n", 1, entity.NewUser()))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Lease, r.Want = rd, LeaseKey{1}, rh
	for name, try := range map[string]func() error{
		"an NFS open": func() error {
			_, err := f.open(n, file, keep(r, entity.DurabilityDurable, s1, time.Minute, 1))
			return err
		},
		"with no keys": func() error {
			q := r
			q.Durability = entity.DurabilityPersistent
			_, err := f.open(a, file, q)
			return err
		},
		"no Timeout": func() error { _, err := f.open(a, file, keep(r, entity.DurabilityPersistent, s1, 0, 1)); return err },
		"of a stream": func() error {
			q := keep(r, entity.DurabilityPersistent, s1, time.Minute, 1)
			q.Stream = entity.NewFileID()
			_, err := f.open(a, file, q)
			return err
		},
	} {
		if err := try(); !errors.Is(err, ErrInvalid) {
			t.Errorf("a kept open %s: %v, want ErrInvalid", name, err)
		}
	}
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	if got := f.stored(t, file); len(got) != 1 || got[0].ID != o.ID || got[0].Durability != entity.DurabilityPersistent || got[0].Session != s1 || string(got[0].Name) != "disk" ||
		got[0].Lease == nil || got[0].Lease.State != 3 {
		t.Fatalf("stored after a persistent open: %+v, want it with its lease at SMB's read and handle, 3", got)
	}
	f.close(t, a, o)
	if got := f.stored(t, file); len(got) != 0 {
		t.Errorf("stored after its close: %+v", got)
	}
}

// A persistent open survives a restart: installed when its shard is begun,
// before anything there is answered, with its deny mode, its locks, its
// suspended times and its entry's current name, as disconnected. It outlives grace until its
// Timeout, reconnects as it was, and closes when its Timeout passes after a
// disconnect of the session it joined.
func TestPersistentOpenSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk.vhdx")
	r.Access, r.Deny = rd|wr, wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, 300*time.Second, 1))
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10), x(20, 10)}})
	if _, err := f.Locks().Unlock(ctx, a, file, byID(o), smbOwner(a, o), []ByteRange{br(0, 10)}, Seqs{}); err != nil {
		t.Fatal(err)
	}
	if err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err)
	}
	if err := f.Opens().Renamed(ctx, file, r.Via, r.Name, r.Via, []byte("moved.vhdx")); err != nil {
		t.Fatal(err)
	}

	g := f.restart(t)
	g.renew(t, b)
	g.idle(120 * time.Second)
	if _, err := g.open(b, file, OpenRequest{Access: wr}); !errors.Is(err, ErrShareViolation) {
		t.Errorf("a write open past grace, inside the Timeout: %v, want ErrShareViolation", err)
	}
	anon := func(off uint64) error {
		return g.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, State: StateRef{Shard: file.Shard}, Handle: handle(file.ID)}, br(off, 1), false)
	}
	if err := anon(25); !errors.Is(err, ErrLocked) {
		t.Errorf("an anonymous read across the installed lock: %v, want ErrLocked", err)
	}
	if err := anon(5); err != nil {
		t.Errorf("an anonymous read across the unlocked range: %v", err)
	}
	back, err := g.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s2)
	if err != nil {
		t.Fatalf("reconnect after the restart: %v", err)
	}
	if back.Open.Suspended != entity.TimeModify || back.Open.Mode.Deny != wr || string(back.Open.Name) != "moved.vhdx" {
		t.Errorf("reconnected %+v; want Modify suspended, deny-write and its entry's new name", back.Open)
	}
	if got := g.stored(t, file); len(got) != 1 || got[0].Session != s2 {
		t.Errorf("stored after the reconnect: %+v, want it naming s2", got)
	}
	g.disconnect(t, a, s2)
	g.idle(300 * time.Second)
	if got := g.stored(t, file); len(got) != 0 {
		t.Errorf("stored after the Timeout closed it: %+v", got)
	}
	if _, err := g.open(b, file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open after the Timeout: %v", err)
	}
}

// A lease survives a restart inside the persistent open it covers, with its
// kind, epoch and version, and the kind an answer to its recall leaves it:
// each is written with the open.
func TestPersistentLeaseSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Epoch, r.Want = rd|wr, LeaseKey{5}, ptr[uint16](3), rwh
	res, err := f.Opens().Open(ctx, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	if err != nil || res.Grant == nil || res.Grant.Kind != rwh {
		t.Fatalf("persistent open: %+v, %v; want a lease rwh", res, err)
	}
	m := ReconnectMatch{Create: [16]byte{1}, Lease: LeaseKey{5}}
	f = f.restart(t)
	f.renew(t, b)
	back, err := f.reconnect(a, res.Open, m, s2)
	if gr := back.Grant; err != nil || gr == nil || gr.Kind != rwh || gr.Epoch != res.Grant.Epoch || !gr.V2 {
		t.Fatalf("reconnected under %+v, %v; want the lease at rwh, epoch %d, v2", back.Grant, err, res.Grant.Epoch)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatalf("b's open: %v, want a recall", err)
	}
	rc := f.sink.takeRecalls()
	if len(rc) != 1 {
		t.Fatalf("recalls %+v", rc)
	}
	if err := f.ack(a, back.Grant, rc[0].to); err != nil {
		t.Fatal(err)
	}
	f.disconnect(t, a, s2)
	g := f.restart(t)
	back, err = g.reconnect(a, res.Open, m, s3)
	if err != nil || back.Grant == nil || back.Grant.Kind != rc[0].to {
		t.Errorf("reconnected under %+v, %v; want the lease at %v", back.Grant, err, rc[0].to)
	}
}

// Grace waits for a client that recovers by reconnecting until each
// persistent open installed for it has reconnected or closed at its Timeout.
func TestGraceWaitsForReconnects(t *testing.T) {
	f := newFixture(t)
	a, c := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-c"))
	b := f.caller(t, smbMachine("pc-b"))
	fa, ra := f.fileIn(t, "a")
	fc, rc := f.fileIn(t, "c")
	ra.Access, rc.Access = rd, rd
	oa := f.mustOpen(t, a, fa, keep(ra, entity.DurabilityPersistent, s1, 300*time.Second, 1))
	f.mustOpen(t, c, fc, keep(rc, entity.DurabilityPersistent, s1, 30*time.Second, 2))
	g := f.restart(t)
	g.renew(t, b)
	other := FileKey{Share: fa.Share, Shard: fa.Shard, ID: entity.NewFileID()}
	if _, err := g.open(b, other, OpenRequest{Access: rd}); !errors.Is(err, ErrGrace) {
		t.Fatalf("an open before any reconnect: %v, want ErrGrace", err)
	}
	if _, err := g.reconnect(a, oa, ReconnectMatch{Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	if _, err := g.open(b, other, OpenRequest{Access: rd}); !errors.Is(err, ErrGrace) {
		t.Errorf("an open while c's open is neither reconnected nor timed out: %v, want ErrGrace", err)
	}
	g.idle(40 * time.Second)
	if _, err := g.open(b, other, OpenRequest{Access: rd}); err != nil {
		t.Errorf("an open once a reconnected and c's open timed out, before grace's 90 s: %v", err)
	}
}

// A flush through an SMB open fails ErrLost once for each loss since the
// open was made or last flushed; a reconnect does not clear it.
func TestFlushedReportsALossOnce(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	o1 := f.mustOpen(t, a, file, keep(OpenRequest{Access: rd, Loss: 4}, entity.DurabilityDurable, s1, time.Minute, 1))
	o2 := f.mustOpen(t, a, file, OpenRequest{Access: rd, Loss: 4})
	late := f.mustOpen(t, a, file, OpenRequest{Access: rd, Loss: 5})
	f.disconnect(t, a, s1)
	if _, err := f.reconnect(a, o1, ReconnectMatch{Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	flush := func(o Open, loss uint64) error { return f.Opens().Flushed(ctx, a, o.ID, loss) }
	for _, o := range []Open{o1, o2} {
		if err := flush(o, 5); !errors.Is(err, ErrLost) {
			t.Errorf("open %d's first flush after the loss: %v, want ErrLost", o.ID, err)
		}
		if err := flush(o, 5); err != nil {
			t.Errorf("open %d's second flush: %v", o.ID, err)
		}
	}
	if err := flush(late, 5); err != nil {
		t.Errorf("a flush through an open made after the loss: %v", err)
	}
	if err := f.Opens().Flushed(ctx, Caller{a.Client, entity.NewUser(), entity.ProtocolSMB}, o2.ID, 6); !errors.Is(err, ErrNotYours) {
		t.Errorf("a flush by another principal: %v, want ErrNotYours", err)
	}
	if err := flush(o2, 6); !errors.Is(err, ErrLost) {
		t.Errorf("the opener's flush after another principal's: %v, want ErrLost", err)
	}
}

// A lease cut to none when its recall passes its deadline is written so,
// and a restart installs it at none.
func TestPersistentLeaseKeepsItsTimeout(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Want = rd|wr, LeaseKey{5}, rwh
	res, err := f.Opens().Open(ctx, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	if err != nil || res.Grant == nil {
		t.Fatalf("persistent open: %+v, %v", res, err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatalf("b's open: %v, want a recall", err)
	}
	f.pass(t, coversDeadline+time.Second)
	g := f.restart(t)
	back, err := g.reconnect(a, res.Open, ReconnectMatch{Create: [16]byte{1}, Lease: LeaseKey{5}}, s2)
	if err != nil || back.Grant == nil || back.Grant.Kind != GrantNone {
		t.Errorf("reconnected under %+v, %v; want the lease at none", back.Grant, err)
	}
}

// Two persistent opens under one lease key come back under one lease, which
// counts once in its client's budget; an unlink that writes the file's
// records keeps the lease in them.
func TestPersistentOpensShareTheirLease(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Want = rd, LeaseKey{5}, rh
	o1 := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	o2 := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 2))
	f.unlink(t, file, "doc")
	g := f.restart(t)
	b1, err1 := g.reconnect(a, o1, ReconnectMatch{Create: [16]byte{1}, Lease: LeaseKey{5}}, s2)
	b2, err2 := g.reconnect(a, o2, ReconnectMatch{Create: [16]byte{2}, Lease: LeaseKey{5}}, s2)
	if err1 != nil || err2 != nil || b1.Grant == nil || b2.Grant == nil || b1.Grant.ID != b2.Grant.ID || b1.Grant.Kind != rh {
		t.Fatalf("reconnected under %+v and %+v (%v, %v); want one lease at rh", b1.Grant, b2.Grant, err1, err2)
	}
	if n := g.granted[a.Client]; n != 1 {
		t.Errorf("the client's budget counts %d grants, want 1", n)
	}
}

// Only a persistent open of a client the shard's records name is installed:
// the record an unlink wrote for a volatile open, and a persistent record of
// a client with no record, stay in the store, for grace's end, and hold
// nothing.
func TestOnlyPersistentOpensAreInstalled(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Deny = rd, wr
	f.mustOpen(t, a, file, r)
	f.unlink(t, file, "doc")
	orphan, _ := f.fileIn(t, "orphan")
	if err := f.fs.Store.Opens().Put(ctx, orphan.Share, entity.OpenRecord{ID: 1 << 30, Client: 999, File: orphan.ID, Access: rd, Deny: wr,
		Durability: entity.DurabilityPersistent, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	g := f.restart(t)
	g.renew(t, b)
	g.idle(2 * leasePeriod)
	if _, err := g.open(b, file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open against the lost volatile open's deny-write: %v", err)
	}
	if got := g.stored(t, file); len(got) != 1 {
		t.Errorf("stored after grace: %+v, want the unlink's record kept", got)
	}
	if _, err := g.open(b, orphan, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open against the unnamed client's deny-write record: %v", err)
	}
}

// failing is a store whose writes fail while fail is set, and whose reads
// fail from the readAt'th one counted from zero while readAt is positive.
type failing struct {
	kv.KV
	fail          atomic.Bool
	readAt, reads atomic.Int64
}

func (s *failing) View(ctx context.Context, fn func(kv.Reader) error) error {
	if at := s.readAt.Load(); at > 0 && s.reads.Add(1) >= at {
		return errWrite
	}
	return s.KV.View(ctx, fn)
}

var errWrite = errors.New("store write refused")

func (s *failing) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if s.fail.Load() {
		return errWrite
	}
	return s.KV.Update(ctx, fn)
}

// failingFS is a fixture's store over a failing store.
func failingFS(t *testing.T) (*kvstoretest.FS, *failing) {
	var st *failing
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		st = &failing{KV: db}
		return st
	})
	return fs, st
}

// A shard whose install fails part way holds nothing of it: a retry
// installs each persistent open once.
func TestFailedInstallLeavesNothing(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	for _, name := range []string{"a", "b"} {
		file, r := f.fileIn(t, name)
		r.Access = rd
		f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1))
	}
	f.Stop()
	for at := int64(1); ; at++ {
		g := unstarted(t, fs, f.clock)
		if err := g.Start(ctx, g.sink, g.order.order); err != nil {
			t.Fatal(err)
		}
		st.reads.Store(0)
		st.readAt.Store(at)
		err := g.Recovery().ServeShard(ctx, fs.Share.ID, 0, instances.Add(1))
		st.readAt.Store(0)
		if err == nil {
			break // every read of the install passed
		}
		if err := g.Recovery().ServeShard(ctx, fs.Share.ID, 0, instances.Add(1)); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		opens := 0
		for _, h := range g.refs {
			if h.typ == StateOpen {
				opens++
			}
		}
		if opens != 2 {
			t.Fatalf("failing read %d: %d opens installed after the retry, want 2", at, opens)
		}
		g.Stop()
	}
}

// A refused write of a persistent open's change answers the store's error
// and changes nothing the table holds: an open, a reconnect, a suspended
// time, a lock and an unlock.
func TestRefusedWriteChangesNothing(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "disk")
	f.mustOpen(t, a, file, OpenRequest{Access: rd}) // the shard marks are written
	f.mustOpen(t, b, file, OpenRequest{Access: rd})
	r.Access, r.Deny = rd|wr, wr
	pr := keep(r, entity.DurabilityPersistent, s1, time.Minute, 1)

	st.fail.Store(true)
	if _, err := f.open(a, file, pr); !errors.Is(err, errWrite) {
		t.Fatalf("a persistent open over a refusing store: %v, want its error", err)
	}
	st.fail.Store(false)
	if ob, err := f.open(b, file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open after the refused deny-write open: %v", err)
	} else {
		f.close(t, b, ob)
	}

	o := f.mustOpen(t, a, file, pr)
	st.fail.Store(true)
	errs := map[string]error{
		"Update": f.Opens().Update(ctx, a, o.ID, OpenUpdate{Suspend: entity.TimeModify}),
		"Lock":   errOf(f.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}})),
	}
	st.fail.Store(false)
	f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(20, 10)}})
	st.fail.Store(true)
	_, errs["Unlock"] = f.Locks().Unlock(ctx, a, file, byID(o), smbOwner(a, o), []ByteRange{br(20, 10)}, Seqs{})
	st.fail.Store(false)
	for name, err := range errs {
		if !errors.Is(err, errWrite) {
			t.Errorf("%s over a refusing store: %v, want its error", name, err)
		}
	}
	if h := f.holder(t, file, br(0, 10)); h != nil {
		t.Errorf("the refused lock is held: %+v", h)
	}
	if h := f.holder(t, file, br(20, 10)); h == nil {
		t.Error("the refused unlock released its range")
	}

	f.disconnect(t, a, s1)
	st.fail.Store(true)
	if _, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s2); !errors.Is(err, errWrite) {
		t.Errorf("a reconnect over a refusing store: %v, want its error", err)
	}
	st.fail.Store(false)
	back, err := f.reconnect(a, o, ReconnectMatch{Create: [16]byte{1}}, s3)
	if err != nil || back.Open.Suspended != 0 {
		t.Errorf("the reconnect after: %+v, %v; want it, with no time suspended", back.Open, err)
	}
}
