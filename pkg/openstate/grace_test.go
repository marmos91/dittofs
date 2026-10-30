package openstate

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

// nfsAs is an NFSv4 client that recovers as k says: status for NFSv4.1,
// refusal for NFSv4.0.
func nfsAs(name string, k entity.RecoveryKind) Client {
	c := nfs(name, 1, entity.NewUser())
	c.Recovery = k
	return c
}

// restart stops f's table and starts another over its store, every test
// shard begun under a new grace instance.
func (f *fixture) restart(t *testing.T) *fixture {
	t.Helper()
	f.Stop()
	return reopen(t, f.fs, f.clock)
}

// renew renews each client, as each one's first request after a restart
// does, failing the test on a refusal.
func (f *fixture) renew(t *testing.T, cs ...Caller) {
	t.Helper()
	for _, c := range cs {
		if _, err := f.Renew(ctx, c.Client); err != nil {
			t.Fatalf("Renew %d: %v", c.Client, err)
		}
	}
}

func in(s ShardID) FileKey { return FileKey{Shard: s, ID: entity.NewFileID()} }

// After a restart only a reclaim is granted, by a client the shard's records
// name, and none that conflicts with one reclaimed before it; every other
// request for state is ErrGrace, a file nobody held included, until grace
// ends on its own a lease period after it began.
func TestGraceAdmitsOnlyReclaims(t *testing.T) {
	f := newFixture(t)
	c, d := f.caller(t, nfsAs("c", entity.RecoverStatus)), f.caller(t, nfsAs("d", entity.RecoverStatus))
	file, idle := in(5), in(5)
	oc := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo")})
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(oc), Ranges: []LockRange{x(0, 10)}})
	f.mustOpen(t, d, idle, OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, c, d)
	for what, err := range map[string]error{
		"a conflicting lock":           errOf(g.lock(d, file, LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}})),
		"a lock of a file nobody held": errOf(g.lock(d, in(5), LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}})),
		"an open":                      func() error { _, err := g.open(d, file, OpenRequest{Access: rd}); return err }(),
	} {
		if !errors.Is(err, ErrGrace) {
			t.Errorf("%s in grace: %v, want ErrGrace", what, err)
		}
	}

	res, err := g.Opens().Open(ctx, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true, Want: rw})
	if err != nil {
		t.Fatal(err)
	}
	if res.Grant != nil {
		t.Errorf("a reclaim in grace was offered a grant: %+v", *res.Grant)
	}
	again := res.Open
	g.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(again), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if _, err := g.lock(d, file, LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(5, 1)}, Wait: WaitCallback, Reclaim: true}); !errors.Is(err, ErrReclaimConflict) {
		t.Errorf("a second client's reclaim of the range: %v, want ErrReclaimConflict", err)
	}
	if h := g.holder(t, file, br(0, 10)); h == nil || h.Owner.Client != c.Client {
		t.Errorf("holder after both reclaims: %+v, want c's", h)
	}
	if _, err := g.open(d, file, OpenRequest{Access: rd, Deny: wr, Reclaim: true}); !errors.Is(err, ErrReclaimConflict) {
		t.Errorf("a reclaimed open denying the write c reclaimed: %v, want ErrReclaimConflict", err)
	}
	e := g.caller(t, nfsAs("e", entity.RecoverStatus))
	if _, err := g.open(e, file, OpenRequest{Access: rd, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim by a client the records do not name: %v, want ErrNoReclaim", err)
	}

	g.pass(t, leasePeriod-time.Second, c.Client, d.Client)
	if _, err := g.lock(d, in(5), LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("a lock just before grace ends: %v, want ErrGrace", err)
	}
	g.pass(t, time.Second, c.Client, d.Client)
	g.mustLock(t, d, in(5), LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}})
}

// In grace a test of a range another client held before the loss is
// ErrGrace, since that lock is not held again until it is reclaimed; after
// grace it is answered as usual.
func TestTestLockInGrace(t *testing.T) {
	f := newFixture(t)
	c, d := f.caller(t, nfsAs("c", entity.RecoverStatus)), f.caller(t, nfsAs("d", entity.RecoverStatus))
	file := in(5)
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.renew(t, c, d)
	if h, err := g.Locks().TestLock(ctx, d, file, owner(d, "lo"), br(0, 1), true); !errors.Is(err, ErrGrace) {
		t.Errorf("TestLock in grace of a range c held: %+v, %v; want ErrGrace", h, err)
	}
	g.pass(t, leasePeriod, c.Client, d.Client)
	if h, err := g.Locks().TestLock(ctx, d, file, owner(d, "lo"), br(0, 1), true); err != nil || h != nil {
		t.Errorf("TestLock after grace: %+v, %v; want no conflict", h, err)
	}
}

// In grace, anonymous I/O waits for its end in a shard whose records name a
// client that can hold a deny mode or a mandatory lock, and not in one that
// names only NLM hosts, which hold neither. A shard whose records name only
// SMB clients ends its grace at once: they reconnect no persistent open, and
// reclaim nothing else.
func TestAnonymousIOWaitsForGrace(t *testing.T) {
	f := newFixture(t)
	sc := smb("pc")
	sc.Recovery = entity.RecoverReconnect
	n, s, h := f.caller(t, nfsAs("n", entity.RecoverStatus)), f.caller(t, sc), f.host(t, "h", 1)
	file, sfile, hfile := in(5), in(7), in(1)
	f.mustOpen(t, n, file, OpenRequest{Access: rd, Deny: wr})
	o := f.mustOpen(t, s, sfile, OpenRequest{Access: rd | wr})
	f.mustLock(t, s, sfile, LockReq{Owner: smbOwner(s, o), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, hfile, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	write := func(file FileKey) error {
		// The reference carries no shard: the file's key names it.
		return g.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, br(0, 1), true)
	}
	if err := write(file); !errors.Is(err, ErrGrace) {
		t.Errorf("an anonymous write where an NFSv4 client held a deny mode: %v, want ErrGrace", err)
	}
	if err := write(hfile); err != nil {
		t.Errorf("an anonymous write where only an NLM host held a lock: %v", err)
	}
	if err := write(sfile); err != nil {
		t.Errorf("an anonymous write where only an SMB client held a lock: %v", err)
	}
	for _, what := range []ChangeMask{ChangeRemove, ChangeRename} {
		if err := g.Checks().CheckChange(ctx, Caller{}, file, nil, what); !errors.Is(err, ErrGrace) {
			t.Errorf("change %d where an NFSv4 client held a deny mode: %v, want ErrGrace", what, err)
		}
	}
	if err := g.Checks().CheckChange(ctx, Caller{}, file, nil, ChangeAttrs); err != nil {
		t.Errorf("a set-attribute in grace: %v", err)
	}
	if err := g.Checks().CheckChange(ctx, Caller{}, hfile, nil, ChangeRemove); err != nil {
		t.Errorf("a remove where only an NLM host held a lock: %v", err)
	}
	g.clock.advance(leasePeriod)
	if err := write(file); err != nil {
		t.Errorf("an anonymous write after grace: %v", err)
	}
}

// Anonymous I/O waits only while a client that can hold a deny mode is left
// to reclaim: once an NFSv4.1 client has completed, a write passes while
// grace still runs for an NLM host, which holds no deny mode.
func TestAnonymousIOFreedOnceDenyHoldersFinish(t *testing.T) {
	f := newFixture(t)
	c, h, z := f.caller(t, nfsAs("c", entity.RecoverStatus)), f.host(t, "h", 1), f.caller(t, nfsAs("z", entity.RecoverStatus))
	file := in(5)
	f.mustOpen(t, c, file, OpenRequest{Access: rd, Deny: wr})
	f.mustLock(t, h, in(5), LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.renew(t, c, z)
	write := func() error {
		return g.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, br(0, 1), true)
	}
	if err := write(); !errors.Is(err, ErrGrace) {
		t.Errorf("an anonymous write before c completes: %v, want ErrGrace", err)
	}
	if err := g.Recovery().ReclaimComplete(ctx, c, []GraceRef{{5, g.shards[5].instance}}); err != nil {
		t.Fatal(err)
	}
	if err := write(); err != nil {
		t.Errorf("an anonymous write with only the host left to reclaim: %v", err)
	}
	if _, err := g.lock(z, in(5), LockReq{Owner: owner(z, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("a new lock while the host may still reclaim: %v, want ErrGrace", err)
	}
}

// A client that recovers by status is told the grace instances it owes,
// and only those; its reclaim is accepted, and grace waits for it until it
// completes. A completion ends only the instances it names: one begun after
// the client was shown its owed ones stays owed, and keeps its grace.
func TestReclaimCompleteEndsOnlyWhatWasShown(t *testing.T) {
	f := newFixture(t)
	cl := nfsAs("c", entity.RecoverStatus)
	c, other := f.caller(t, cl), f.caller(t, nfsAs("other", entity.RecoverStatus))
	if err := f.Recovery().ReclaimComplete(ctx, c, nil); err != nil { // at mount
		t.Fatal(err)
	}
	a, b := in(5), in(7)
	f.mustOpen(t, c, a, OpenRequest{Access: rd, Owner: owner(c, "oo")})
	held := f.mustOpen(t, c, b, OpenRequest{Access: rd, Owner: owner(c, "oo")})
	if s, _ := f.Renew(ctx, c.Client); s.ReclaimNeeded {
		t.Errorf("before any loss: %+v", s)
	}

	g := f.restart(t)
	g.renew(t, other)
	st, err := g.Renew(ctx, c.Client)
	if err != nil {
		t.Fatal(err)
	}
	shown := st.Reclaim
	want := []GraceRef{{5, g.shards[5].instance}, {7, g.shards[7].instance}}
	slices.SortFunc(shown, func(x, y GraceRef) int { return int(x.Shard) - int(y.Shard) })
	if !st.ReclaimNeeded || !reflect.DeepEqual(shown, want) {
		t.Fatalf("after the restart: %+v, want ReclaimNeeded for %v", st, want)
	}
	g.mustOpen(t, c, a, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true})
	reclaimed := g.mustOpen(t, c, b, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true})
	// None of these ends what c owes: confirming again, another principal's
	// completion, or one naming an instance not yet begun.
	g.connect(t, cl)
	stranger := c
	stranger.Principal = entity.NewUser()
	if err := g.Recovery().ReclaimComplete(ctx, stranger, shown); !errors.Is(err, ErrNotYours) {
		t.Errorf("another principal's completion: %v, want ErrNotYours", err)
	}
	if err := g.Recovery().ReclaimComplete(ctx, c, []GraceRef{{5, shown[0].Instance + 1}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.Renew(ctx, c.Client); len(st.Reclaim) != 2 {
		t.Errorf("owed after completions that end nothing: %+v, want both shards", st)
	}

	// Shard 7 is lost again after c was shown its instance: what it
	// reclaimed there is gone, and its reference stale.
	if err := g.Recovery().ServeShard(ctx, entity.ShareID{}, 7, g.shards[7].instance+100); err != nil {
		t.Fatal(err)
	}
	if err := g.Recovery().TestState(ctx, c, []StateRef{reclaimed.Ref, held.Ref}); !errors.Is(err[0], ErrStaleState) || !errors.Is(err[1], ErrStaleState) {
		t.Errorf("references of shard 7 after its second loss: %v, want ErrStaleState", err)
	}
	if _, err := g.lock(other, in(5), LockReq{Owner: owner(other, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("before c completes: %v, want ErrGrace", err)
	}
	if err := g.Recovery().ReclaimComplete(ctx, c, shown); err != nil {
		t.Fatal(err)
	}
	g.mustLock(t, other, in(5), LockReq{Owner: owner(other, "lo"), Ranges: []LockRange{x(0, 1)}})
	if _, err := g.lock(other, in(7), LockReq{Owner: owner(other, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("in the instance c was not shown: %v, want ErrGrace", err)
	}
	// A completion naming the instance c joined under, which it finished,
	// takes nothing back.
	if err := g.Recovery().ReclaimComplete(ctx, c, []GraceRef{{5, f.shards[5].instance}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.Renew(ctx, c.Client); !reflect.DeepEqual(st.Reclaim, []GraceRef{{7, g.shards[7].instance}}) {
		t.Errorf("after the completion: %+v, want the later instance of shard 7 alone", st)
	}
}

// Only a client that recovers by status finishes by completing: an NLM
// host's or an NFSv4.0 client's completion ends no grace.
func TestReclaimCompleteOnlyByStatus(t *testing.T) {
	f := newFixture(t)
	h, c40, z := f.host(t, "h", 1), f.caller(t, nfsAs("c40", entity.RecoverRefusal)), f.caller(t, nfsAs("z", entity.RecoverStatus))
	f.mustLock(t, h, in(5), LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	f.mustOpen(t, c40, in(7), OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, z)
	for _, c := range []struct {
		who Caller
		s   ShardID
	}{{h, 5}, {c40, 7}} {
		s := c.s
		if err := g.Recovery().ReclaimComplete(ctx, c.who, []GraceRef{{s, g.shards[s].instance}}); err != nil {
			t.Fatal(err)
		}
		if _, err := g.lock(z, in(s), LockReq{Owner: owner(z, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
			t.Errorf("a new lock in shard %d after its only client completed: %v, want ErrGrace", s, err)
		}
	}
}

// A client that recovers by refusal is refused on every call naming it
// until it connects again under its name and Boot, which keeps its ID; a
// reference it held is stale, and its reclaim is then granted. It never
// counts as finished, so grace runs until it expires.
func TestRefusalClientConnectsAgain(t *testing.T) {
	f := newFixture(t)
	cl := nfsAs("c40", entity.RecoverRefusal)
	c, done := f.caller(t, cl), f.caller(t, nfsAs("done", entity.RecoverStatus))
	c2 := f.caller(t, nfsAs("c40-2", entity.RecoverRefusal))
	file := in(5)
	f.mustLock(t, c2, in(5), LockReq{Owner: owner(c2, "lo"), Ranges: []LockRange{x(0, 1)}})
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo")})
	lk := f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	f.mustOpen(t, done, in(5), OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, done)
	if err := g.Recovery().ReclaimComplete(ctx, done, []GraceRef{{5, g.shards[5].instance}}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the first adopts the client after the restart
		if _, err := g.Renew(ctx, c.Client); !errors.Is(err, ErrStaleClient) {
			t.Errorf("Renew: %v, want ErrStaleClient", err)
		}
	}
	if _, err := g.open(c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true}); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a reclaim before connecting again: %v, want ErrStaleClient", err)
	}
	// c2 has sent nothing since the restart.
	if _, err := g.Locks().TestLock(ctx, c2, file, owner(c2, "lo"), br(0, 1), true); !errors.Is(err, ErrStaleClient) {
		t.Errorf("TestLock: %v, want ErrStaleClient", err)
	}
	if err := g.Locks().ReleaseLockOwner(ctx, c2, owner(c2, "lo")); !errors.Is(err, ErrStaleClient) {
		t.Errorf("ReleaseLockOwner: %v, want ErrStaleClient", err)
	}
	if err := g.Recovery().TestState(ctx, c, []StateRef{lk.Ref})[0]; !errors.Is(err, ErrStaleState) {
		t.Errorf("the lock state held before: %v, want ErrStaleState", err)
	}

	if id := g.connect(t, cl); id != c.Client {
		t.Fatalf("connecting again: ID %d, want %d", id, c.Client)
	}
	g.renew(t, c)
	again := g.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true})
	g.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(again), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if _, err := g.lock(done, in(5), LockReq{Owner: owner(done, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("with the refusal client reclaimed: %v, want ErrGrace", err)
	}
	if err := g.Expire(ctx, c.Client); err != nil {
		t.Fatal(err)
	}
	if err := g.Expire(ctx, c2.Client); err != nil {
		t.Fatal(err)
	}
	g.mustLock(t, done, in(5), LockReq{Owner: owner(done, "lo"), Ranges: []LockRange{x(0, 1)}})
}

// A shard where NLM hosts held locks raises the restart-notice number before
// its grace begins and tells each host, with its adapter bytes, as it tells
// an SMB client; the hosts' reclaims are granted, and a host revoked before
// the restart, or whose record is ended, is told nothing and reclaims
// nothing.
func TestShardLossToldToHosts(t *testing.T) {
	f := newFixture(t)
	h1, h2, gone, ended := f.host(t, "h1", 1), f.host(t, "h2", 1), f.host(t, "gone", 1), f.host(t, "ended", 1)
	sc := smb("pc")
	sc.Recovery = entity.RecoverReconnect
	s := f.caller(t, sc)
	file := in(5)
	for i, h := range []Caller{h1, h2, gone, ended} {
		f.mustLock(t, h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(uint64(i)*10, 10)}})
	}
	f.mustOpen(t, s, file, OpenRequest{Access: rd})
	if _, err := f.AddSession(ctx, h1.Client, entity.NodeID{}, nil, []byte("h1-addr")); err != nil {
		t.Fatal(err)
	}
	if err := f.Revoke(ctx, gone.Client); err != nil {
		t.Fatal(err)
	}
	rec, err := f.fs.Store.Clients().Get(ctx, ended.Client)
	if err != nil {
		t.Fatal(err)
	}
	next := rec
	next.Ended = true
	if _, err := f.fs.Store.Clients().Put(ctx, rec, next); err != nil {
		t.Fatal(err)
	}
	before, err := f.fs.Store.NSMState(ctx)
	if err != nil {
		t.Fatal(err)
	}

	f.Stop()
	g := unstarted(t, f.fs, f.clock)
	if err := g.Start(ctx, g.sink, g.order.order); err != nil {
		t.Fatal(err)
	}
	g.sink.takeRevoked()
	var toldAt []uint32 // the restart-notice number as each client is told
	g.sink.onRevoke = func(Revocation) {
		n, _ := f.fs.Store.NSMState(ctx)
		toldAt = append(toldAt, n)
	}
	if err := g.Recovery().ServeShard(ctx, entity.ShareID{}, 5, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	for _, n := range toldAt {
		if n <= before {
			t.Errorf("restart-notice number %d as a client was told, %d before", n, before)
		}
	}
	got := map[entity.ClientID]Revocation{}
	for _, r := range g.sink.takeRevoked() {
		if len(r.Adapter) == 0 {
			r.Adapter = nil
		}
		got[r.Client] = r
	}
	wantTold := map[entity.ClientID]Revocation{
		h1.Client: {Client: h1.Client, Shard: 5, Reason: RevokedShardState, Adapter: []byte("h1-addr")},
		h2.Client: {Client: h2.Client, Shard: 5, Reason: RevokedShardState},
		s.Client:  {Client: s.Client, Shard: 5, Reason: RevokedShardState},
	}
	if !reflect.DeepEqual(got, wantTold) {
		t.Errorf("Revoked %+v, want %+v", got, wantTold)
	}
	raised, _ := f.fs.Store.NSMState(ctx)
	for _, s := range testShards[:2] {
		if err := g.Recovery().ServeShard(ctx, entity.ShareID{}, s, instances.Add(1)); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := f.fs.Store.NSMState(ctx); n != raised {
		t.Errorf("restart-notice number %d after shards no host held state in began, %d before", n, raised)
	}
	g.renew(t, h1, h2)
	g.mustLock(t, h1, file, LockReq{Owner: owner(h1, "p"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if _, err := g.lock(h2, file, LockReq{Owner: owner(h2, "p"), Ranges: []LockRange{x(100, 10)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("a host's new lock in grace: %v, want ErrGrace", err)
	}
	again := g.host(t, "gone", 1)
	if _, err := g.lock(again, file, LockReq{Owner: owner(again, "p"), Ranges: []LockRange{x(20, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("the revoked host's reclaim: %v, want ErrNoReclaim", err)
	}
}

// A revoked client's shard list is emptied, so after a restart its reclaim
// is refused rather than taking back the lock another client took since.
func TestRevokedClientReclaimsNothing(t *testing.T) {
	f := newFixture(t)
	x1, y := f.caller(t, nfsAs("x", entity.RecoverStatus)), f.caller(t, nfsAs("y", entity.RecoverStatus))
	file := in(5)
	f.mustLock(t, x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}})
	if err := f.Revoke(ctx, x1.Client); err != nil {
		t.Fatal(err)
	}
	f.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.renew(t, x1, y)
	if _, err := g.lock(x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("the revoked client's reclaim: %v, want ErrNoReclaim", err)
	}
	g.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	// A revoke in grace ends the client's reclaim there too.
	if err := g.Revoke(ctx, y.Client); err != nil {
		t.Fatal(err)
	}
	if _, err := g.lock(y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim after a revoke in grace: %v, want ErrNoReclaim", err)
	}
}

// A reference whose record is gone is stale in grace and when minted under
// another grace instance than the shard's, and bad otherwise: across 40
// restarts, only the one whose instance aliases the first's low bits finds
// it bad.
func TestMissJudgedByGraceInstance(t *testing.T) {
	f := newFixture(t)
	const s ShardID = 9
	serve := func(g *fixture, inst uint64) {
		if err := g.Recovery().ServeShard(ctx, entity.ShareID{}, s, inst); err != nil {
			t.Fatal(err)
		}
	}
	serve(f, 100)
	ref := mintIn(t, f.Table, entity.NewFileID(), 1, s)
	for k := uint64(1); k <= 40; k++ {
		f = f.restart(t)
		serve(f, 100+k)
		want := ErrStaleState
		if k == 32 {
			want = ErrBadState
		}
		if err := f.Recovery().TestState(ctx, Caller{Client: 1}, []StateRef{ref})[0]; !errors.Is(err, want) {
			t.Errorf("restart %d: %v, want %v", k, err, want)
		}
	}
	// In grace a miss is stale whatever its instance.
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	f.mustOpen(t, c, in(s), OpenRequest{Access: rd})
	f = f.restart(t)
	serve(f, 100+32+32)
	if err := f.Recovery().TestState(ctx, Caller{Client: 1}, []StateRef{ref})[0]; !errors.Is(err, ErrStaleState) {
		t.Errorf("an aliasing reference in grace: %v, want ErrStaleState", err)
	}
}

// A reclaim in a shard that lost nothing is answered with what the client
// still holds, its delegation included, and refused for more than it holds;
// in grace a reclaimed open is offered no delegation, the shard having lost
// its grants, and its owner needs no confirm.
func TestReclaimWhereNothingWasLost(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file := in(5)
	res, err := f.Opens().Open(ctx, c, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Want: GrantRead})
	if err != nil || res.Grant == nil {
		t.Fatalf("Open: %+v, %v; want a delegation", res, err)
	}
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(res.Open), Ranges: []LockRange{sh(0, 10)}})

	again, err := f.Opens().Open(ctx, c, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true})
	if err != nil || again.Open.Ref.Token != res.Open.Ref.Token || again.Grant == nil || again.Grant.ID != res.Grant.ID {
		t.Errorf("a reclaim of the held open: %+v, %v; want it with its delegation", again, err)
	}
	if _, err := f.open(c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim of more access than held: %v, want ErrNoReclaim", err)
	}
	stranger := c
	stranger.Principal = entity.NewUser()
	if _, err := f.open(stranger, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true}); !errors.Is(err, ErrNotYours) {
		t.Errorf("a reclaim of the open by another principal: %v, want ErrNotYours", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(res.Open), Ranges: []LockRange{sh(2, 3)}, Reclaim: true}); err != nil {
		t.Errorf("a reclaim of a held lock: %v", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(res.Open), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim of an exclusive lock over a shared one: %v, want ErrNoReclaim", err)
	}

	g := f.restart(t)
	g.renew(t, c)
	one := uint32(1)
	got, err := g.Opens().Open(ctx, c, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Want: GrantRead, Reclaim: true, Seqs: Seqs{Owner: &one}})
	if err != nil || got.Grant != nil || got.Confirm {
		t.Errorf("a reclaim in grace: %+v, %v; want granted, with no delegation and no confirm", got, err)
	}
}

// A call that mints state in a shard the table has not begun serving is
// refused and writes nothing; once the shard is begun it is granted.
func TestUnservedShardMintsNothing(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file := in(9)
	if _, err := f.open(c, file, OpenRequest{Access: rd}); delayedFor(err) != DelayFrozen {
		t.Errorf("an open in a shard not begun: %v, want ErrDelay (DelayFrozen)", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: owner(c, "lo"), Ranges: []LockRange{x(0, 1)}}); delayedFor(err) != DelayFrozen {
		t.Errorf("a lock in a shard not begun: %v, want ErrDelay (DelayFrozen)", err)
	}
	if rec, _ := f.Record(ctx, c.Client); len(rec.Shards) != 0 {
		t.Errorf("shards listed by the refusals: %+v", rec.Shards)
	}
	if err := f.Recovery().ServeShard(ctx, entity.ShareID{}, 9, 1); err != nil {
		t.Fatal(err)
	}
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd})
	if o.Ref.Instance != 1 {
		t.Errorf("reference minted under instance bits %d, want 1", o.Ref.Instance)
	}
	// Begun again under the same instance, as a handover is, it lost
	// nothing: no grace.
	if err := f.Recovery().ServeShard(ctx, entity.ShareID{}, 9, 1); err != nil {
		t.Fatal(err)
	}
	f.mustOpen(t, c, in(9), OpenRequest{Access: rd})
}

// A rename of the entry an open was made through moves the open's name, so
// the deferred unlink removes the entry's current name, and the directory
// it was made through follows it.
func TestRenameMovesTheDeferredUnlink(t *testing.T) {
	f := newFixture(t)
	p := f.caller(t, smb("p-pc"))
	file, d1, d2 := anyFile(), entity.NewFileID(), entity.NewFileID()
	o := f.mustOpen(t, p, file, OpenRequest{Via: d1, Name: []byte("a"), Access: rd | del, DeleteOnClose: true})
	link := f.mustOpen(t, p, file, OpenRequest{Via: d1, Name: []byte("c"), Access: rd}) // through another link
	if err := f.Opens().Renamed(ctx, file, d1, []byte("a"), d2, []byte("b")); err != nil {
		t.Fatal(err)
	}
	f.idx.Lock()
	n1, n2 := f.via[d1], f.via[d2]
	f.idx.Unlock()
	if n1 != 1 || n2 != 1 {
		t.Errorf("opens made through each directory: %d and %d, want 1 and 1", n1, n2)
	}
	f.close(t, p, link)
	if r := f.close(t, p, o); r.Unlink == nil || r.Unlink.Parent != d2 || string(r.Unlink.Name) != "b" {
		t.Errorf("the deferred unlink: %+v, want b in the directory it was renamed to", r.Unlink)
	}
}

// A client that ends in grace, by a reboot under a new Boot or a destroy, is
// no longer waited for: grace ends early once the others have finished.
func TestEndedClientEndsGraceEarly(t *testing.T) {
	f := newFixture(t)
	xc, yc := nfsAs("x", entity.RecoverStatus), nfsAs("y", entity.RecoverRefusal)
	xcl, y, z := f.caller(t, xc), f.caller(t, yc), f.caller(t, nfsAs("z", entity.RecoverStatus))
	f.mustOpen(t, xcl, in(5), OpenRequest{Access: rd})
	f.mustOpen(t, y, in(5), OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, z)
	free := func() error {
		_, err := g.lock(z, in(5), LockReq{Owner: owner(z, "lo"), Ranges: []LockRange{x(0, 1)}})
		return err
	}
	xc.Boot = [8]byte{2}
	if _, err := g.Connect(ctx, xc, nil); err != nil {
		t.Fatal(err)
	}
	if err := free(); !errors.Is(err, ErrGrace) {
		t.Errorf("with y still recorded: %v, want ErrGrace", err)
	}
	if err := g.Destroy(ctx, y); err != nil {
		t.Fatal(err)
	}
	if err := free(); err != nil {
		t.Errorf("once x rebooted and y was destroyed: %v, want granted", err)
	}
}

// A lock refused ErrGrace answers its owner's number, so the owner's next
// request is judged as following it.
func TestGraceRefusedLockAdvancesItsOwner(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file, other := in(5), in(5)
	oo := owner(c, "oo")
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: oo, Seqs: num(1)})
	if _, err := f.Opens().ConfirmOpen(ctx, c, refOf(o), num(2)); err != nil {
		t.Fatal(err)
	}
	f.mustOpen(t, c, other, OpenRequest{Access: rd, Owner: oo, Seqs: num(3)})
	g := f.restart(t)
	g.renew(t, c)
	again := g.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: oo, Reclaim: true, Seqs: num(1)})
	_, err := g.lock(c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(again), Ranges: []LockRange{x(0, 1)}, Seqs: Seqs{Owner: ptr(uint32(2)), NewOwner: 1}})
	if !errors.Is(err, ErrGrace) {
		t.Fatalf("a lock in grace: %v, want ErrGrace", err)
	}
	if _, err := g.open(c, other, OpenRequest{Access: rd, Owner: oo, Reclaim: true, Seqs: num(3)}); err != nil {
		t.Errorf("the owner's next request after the refused lock: %v, want granted", err)
	}
}

// A reclaim of locks answered from what the owner holds answers the lock
// owner's number.
func TestHeldLockReclaimAdvancesItsOwner(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file := in(5)
	oo, lo := owner(c, "oo"), owner(c, "lo")
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: oo, Seqs: num(1)})
	if _, err := f.Opens().ConfirmOpen(ctx, c, refOf(o), num(2)); err != nil {
		t.Fatal(err)
	}
	res := f.mustLock(t, c, file, LockReq{Owner: lo, Open: refOf(o), Ranges: []LockRange{x(0, 10)}, Seqs: Seqs{Owner: ptr(uint32(3)), NewOwner: 1}})
	ref := res.Ref
	ref.Seq = 0
	byLock := OpenRef{Kind: RefLocked, State: ref}
	if _, err := f.lock(c, file, LockReq{Owner: lo, Open: byLock, Ranges: []LockRange{x(0, 10)}, Reclaim: true, Seqs: num(2)}); err != nil {
		t.Fatalf("a reclaim of a held lock: %v", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: lo, Open: byLock, Ranges: []LockRange{x(20, 10)}, Seqs: num(3)}); err != nil {
		t.Errorf("the lock owner's next lock after the reclaim: %v, want granted", err)
	}
}

// A reclaim admitted in grace that reaches its file's step after grace has
// ended is judged as one sent after it: of state not held, ErrNoReclaim.
func TestReclaimLandingAfterGrace(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	f.mustOpen(t, c, in(5), OpenRequest{Access: rd, Owner: owner(c, "oo")})
	g := f.restart(t)
	g.renew(t, c)
	g.clock.advance(leasePeriod / 2)
	g.renew(t, c)
	target := in(5) // c never held it
	hold, release := make(chan struct{}), make(chan struct{})
	go func() { _ = g.step(target.ID, func(*entry) error { close(hold); <-release; return nil }) }()
	<-hold
	done := make(chan error, 1)
	go func() {
		_, err := g.open(c, target, OpenRequest{Access: rd | wr, Deny: wr, Owner: owner(c, "oo"), Reclaim: true})
		done <- err
	}()
	fs := &g.files[target.ID[0]]
	for deadline, waiting := time.Now().Add(5*time.Second), false; !waiting; time.Sleep(time.Millisecond) {
		fs.mu.Lock()
		waiting = fs.entries[target.ID].users == 2
		fs.mu.Unlock()
		if !waiting && time.Now().After(deadline) {
			t.Fatal("the reclaim did not wait for the step within 5 s")
		}
	}
	g.clock.advance(leasePeriod / 2) // grace ends while the reclaim waits for the step
	close(release)
	if err := <-done; !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim admitted in grace landing after it: %v, want ErrNoReclaim", err)
	}
}

// A host reclaiming the range a callback grant has just given it, while that
// callback is still on its way, keeps the lock when it then refuses the
// callback: the reclaim answered it granted.
func TestHeldReclaimSettlesAnOfferedGrant(t *testing.T) {
	f := newFixture(t)
	a, h := f.host(t, "a", 1), f.host(t, "h", 1)
	file := anyFile()
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	ws := f.sink.takeWaits()
	if g := granted(t, ws); len(g) != 1 {
		t.Fatalf("after the unlock: granted %v, want h", g)
	}
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if err := f.Locks().WaitAnswered(ctx, h, ws[0].w, true); err != nil {
		t.Fatal(err)
	}
	if f.holder(t, file, br(0, 10)) == nil {
		t.Errorf("h reclaimed the lock, then lost it to its own refused callback")
	}
}

// A stacking host reclaiming the range a callback grant has just given it
// leaves the grant whole: the reclaim names a lock of the range, not the
// grant, so refusing the callback then releases it.
func TestStackingHeldReclaimLeavesAnOfferedGrantWhole(t *testing.T) {
	f := newFixture(t)
	h := f.host(t, "h", 1)
	ho := LockOwner{Client: h.Client, Name: []byte("h"), Stack: true}
	file := anyFile()
	f.mustLock(t, h, file, LockReq{Owner: ho, Ranges: []LockRange{sh(30, 5)}}) // keeps h's record once [0,10) goes
	g := callbackGrant(t, f, file, h, ho, "a", br(0, 10), x(0, 10))
	f.mustLock(t, h, file, LockReq{Owner: ho, Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if err := f.Locks().WaitAnswered(ctx, h, g, true); err != nil {
		t.Fatal(err)
	}
	if l := f.holder(t, file, br(0, 10)); l != nil {
		t.Errorf("h reclaimed [0,10) and refused its grant, yet holds %+v", *l)
	}
}

// A shard served again under a new instance refuses its requests while its
// old state goes, and a file whose opens the loss drops is no longer delete
// pending to a request that waited on the file's step meanwhile.
func TestServedAgainWhileAFileIsStepped(t *testing.T) {
	f := newFixture(t)
	p, c := f.caller(t, smb("p-pc")), f.caller(t, nfsAs("c", entity.RecoverStatus))
	for i := range 20 {
		s := ShardID(100 + i)
		if err := f.Recovery().ServeShard(ctx, entity.ShareID{}, s, instances.Add(1)); err != nil {
			t.Fatal(err)
		}
		file := in(s)
		o := f.mustOpen(t, p, file, OpenRequest{Access: rd | del, DeleteOnClose: true})
		f.mustOpen(t, p, file, OpenRequest{Access: rd | del})
		f.close(t, p, o) // delete pending, the other open staying
		fs := &f.files[file.ID[0]]
		waitUsers := func(n int) {
			for u := 0; u != n; {
				fs.mu.Lock()
				u = fs.entries[file.ID].users
				fs.mu.Unlock()
				runtime.Gosched()
			}
		}
		hold, release := make(chan struct{}), make(chan struct{})
		go func() { _ = f.step(file.ID, func(*entry) error { close(hold); <-release; return nil }) }()
		<-hold
		served := make(chan error, 1)
		go func() { served <- f.Recovery().ServeShard(ctx, entity.ShareID{}, s, instances.Add(1)) }()
		waitUsers(2) // the loss waits for the file's step
		if _, err := f.open(c, in(s), OpenRequest{Access: rd}); delayedFor(err) != DelayFrozen {
			t.Errorf("an open while the shard's old state goes: %v, want ErrDelay (DelayFrozen)", err)
		}
		type seen struct {
			opens   int
			pending bool
		}
		after := make(chan seen, 1)
		go func() {
			_ = f.step(file.ID, func(e *entry) error { after <- seen{len(e.opens), e.pending != nil}; return nil })
		}()
		waitUsers(3)
		close(release)
		if err := <-served; err != nil {
			t.Fatal(err)
		}
		if got := <-after; got.opens == 0 {
			if got.pending {
				t.Errorf("the file after the loss dropped its opens: still delete pending")
			}
			return
		} // else the waiting step ran before the loss: try again
	}
	t.Fatal("the waiting step never ran after the loss")
}

// An NLM host's unlock in grace, leaving it nothing here, keeps its record
// while another shard's grace still waits on its reclaim, which is then
// admitted.
func TestHostRecordOutlivesIdlenessInGrace(t *testing.T) {
	f := newFixture(t)
	h := f.host(t, "h", 1)
	a, b := in(5), in(7)
	f.mustLock(t, h, a, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, b, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	g := f.restart(t)
	g.renew(t, h)
	g.mustLock(t, h, a, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	g.unlock(t, h, a, owner(h, "p"), br(0, 10))
	if _, err := g.lock(h, b, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); err != nil {
		t.Errorf("the reclaim on b after the host's last unlock in grace: %v", err)
	}
}

// In a shard holding an SMB client's deny-write open and an NLM host's lock,
// anonymous I/O after a restart waits for nothing of the SMB client's, while
// the host's lock still holds grace for requests that are no reclaim.
func TestReconnectingClientHoldsNoDenyInGrace(t *testing.T) {
	f := newFixture(t)
	sc := smb("pc")
	sc.Recovery = entity.RecoverReconnect
	s, h, z := f.caller(t, sc), f.host(t, "h", 1), f.caller(t, nfsAs("z", entity.RecoverStatus))
	file := in(5)
	f.mustOpen(t, s, file, OpenRequest{Access: rd | wr, Deny: wr})
	f.mustLock(t, h, in(5), LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	g := f.restart(t)
	g.renew(t, z)
	if err := g.Checks().CheckIO(ctx, z, file, OpenRef{Kind: RefAnonymous}, br(0, 10), true); err != nil {
		t.Errorf("an anonymous write on the SMB client's file in grace: %v", err)
	}
	if _, err := g.lock(z, in(5), LockReq{Owner: owner(z, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("a lock that is no reclaim, while the host may reclaim: %v, want ErrGrace", err)
	}
}

// A client silent through a whole grace expires a lease period after the
// grace began, as if it had renewed once at the restart: the next grace does
// not list it, so it cannot reclaim over a lock granted meanwhile.
func TestSilentThroughGraceExpires(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfsAs("a", entity.RecoverStatus)), f.caller(t, nfsAs("b", entity.RecoverStatus))
	file := in(5)
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.renew(t, b)
	g.pass(t, leasePeriod, b.Client)
	g.sweep(ctx)
	g.mustLock(t, b, file, LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}})

	h := g.restart(t)
	_, err := h.Renew(ctx, a.Client)
	if err == nil {
		_, err = h.lock(a, file, LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	}
	if err == nil {
		t.Error("a client silent through the last grace reclaimed its lock")
	}
	h.renew(t, b)
	if _, err := h.lock(b, file, LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); err != nil {
		t.Errorf("the lock granted after the last grace, reclaimed: %v", err)
	}
}

// A shard not begun yet refuses anonymous I/O and every change with ErrDelay,
// as it refuses an open or a lock: a deny mode held there before the restart
// is not yet known to judge them.
func TestShardNotBegunDelaysIOAndChanges(t *testing.T) {
	f := newFixture(t)
	n := f.caller(t, nfsAs("n", entity.RecoverStatus))
	file := in(5)
	f.mustOpen(t, n, file, OpenRequest{Access: rd, Deny: wr})
	f.Stop()
	g := reopenServing(t, f.fs, f.clock, []ShardID{0})
	if err := g.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, br(0, 1), true); delayedFor(err) != DelayFrozen {
		t.Errorf("an anonymous write in a shard not begun: %v, want ErrDelay (DelayFrozen)", err)
	}
	for _, what := range []ChangeMask{ChangeRemove, ChangeAttrs} {
		if err := g.Checks().CheckChange(ctx, Caller{}, file, nil, what); delayedFor(err) != DelayFrozen {
			t.Errorf("a change %b in a shard not begun: %v, want ErrDelay (DelayFrozen)", what, err)
		}
	}
}

// A reclaim of locks answered from what the owner holds, through the open
// that introduces the lock owner, answers the new lock owner's number as a
// lock granted through it does.
func TestHeldLockReclaimThroughItsOpenAnswersTheLockOwner(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverRefusal))
	file := in(5)
	oo, lo := owner(c, "oo"), owner(c, "lo")
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: oo, Seqs: num(1)})
	if _, err := f.Opens().ConfirmOpen(ctx, c, refOf(o), num(2)); err != nil {
		t.Fatal(err)
	}
	res := f.mustLock(t, c, file, LockReq{Owner: lo, Open: refOf(o), Ranges: []LockRange{x(0, 10)}, Seqs: Seqs{Owner: ptr(uint32(3)), NewOwner: 1}})
	if _, err := f.lock(c, file, LockReq{Owner: lo, Open: refOf(o), Ranges: []LockRange{x(0, 10)}, Reclaim: true, Seqs: Seqs{Owner: ptr(uint32(4)), NewOwner: 2}}); err != nil {
		t.Fatalf("a reclaim of a held lock through its open: %v", err)
	}
	ref := res.Ref
	ref.Seq = 0
	if _, err := f.lock(c, file, LockReq{Owner: lo, Open: OpenRef{Kind: RefLocked, State: ref}, Ranges: []LockRange{x(20, 10)}, Seqs: num(3)}); err != nil {
		t.Errorf("the lock owner's next lock after the reclaim: %v, want granted", err)
	}
}

// Grace ends once only clients that recover by reconnecting are left to
// reclaim, as they reclaim nothing here.
func TestGraceEndsWhenOnlyReconnectingClientsAreLeft(t *testing.T) {
	f := newFixture(t)
	sc := smb("pc")
	sc.Recovery = entity.RecoverReconnect
	s, c := f.caller(t, sc), f.caller(t, nfsAs("c", entity.RecoverStatus))
	f.mustOpen(t, s, in(5), OpenRequest{Access: rd})
	f.mustOpen(t, c, in(5), OpenRequest{Access: rd, Owner: owner(c, "oo")})
	g := f.restart(t)
	st, err := g.Renew(ctx, c.Client)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Recovery().ReclaimComplete(ctx, c, st.Reclaim); err != nil {
		t.Fatal(err)
	}
	if _, err := g.open(c, in(5), OpenRequest{Access: rd, Owner: owner(c, "oo")}); err != nil {
		t.Errorf("an open once only the reconnecting client is left: %v, want granted", err)
	}
}

// An NLM host left idle when grace ends loses its record with its next
// call that leaves it idle, as a host idle outside grace does.
func TestIdleHostRecordGoesAfterGrace(t *testing.T) {
	f := newFixture(t)
	first := f.host(t, "h", 1)
	f.mustLock(t, first, in(5), LockReq{Owner: owner(first, "p"), Ranges: []LockRange{x(0, 10)}})
	g := f.restart(t)
	g.pass(t, leasePeriod)
	h := g.host(t, "h", 1)
	file := in(5)
	g.mustLock(t, h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	g.unlock(t, h, file, owner(h, "p"), br(0, 10))
	if _, err := g.fs.Store.Clients().Get(ctx, h.Client); err == nil {
		t.Error("the idle host's record outlived its last unlock after grace")
	}
}

// A reclaim answered from held state asking more than is held, a wider
// lock range or a wider deny, is ErrNoReclaim.
func TestHeldReclaimOfMoreIsRefused(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file := in(5)
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo")})
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	if _, err := f.lock(c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 20)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim of a wider range than held: %v, want ErrNoReclaim", err)
	}
	if _, err := f.open(c, file, OpenRequest{Access: rd, Deny: wr, Owner: owner(c, "oo"), Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim of a wider deny than held: %v, want ErrNoReclaim", err)
	}
}

// DropShare ends every piece of a share's state with no recall: a blocked
// lock and a watch's held changes go, each holder is told RevokedShare with
// the move's address, and a reference there is ErrBadState with no revoked
// flag. Another share's state stays, and the share's durable records are
// left to the store.
func TestDropShare(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("a", 1, entity.NewUser())), f.caller(t, nfs("b", 1, entity.NewUser()))
	c := f.caller(t, smbMachine("pc-c"))
	if err := f.Recovery().ServeShard(ctx, entity.ShareID{9}, 9, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	other := f.mustOpen(t, b, in(9), OpenRequest{Access: rd})
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	f.mustOpen(t, c, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	src, _ := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	if _, err := f.Opens().CloneOpen(ctx, src, f.cloned(t, src, dst)); err != nil {
		t.Fatal(err)
	}
	held, dir := in(1), in(1)
	oa := f.mustOpen(t, a, held, OpenRequest{Access: rd | wr})
	f.mustLock(t, a, held, LockReq{Owner: owner(a, "lo"), Open: refOf(oa), Ranges: []LockRange{x(0, 10)}})
	if res, err := f.lock(b, held, LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); err != nil || !res.Waiting {
		t.Fatalf("the blocked lock: %+v, %v", res, err)
	}
	f.watch(t, b, dir, false, names)
	f.changed(dir, ChangeAddEntry, "first", false)
	f.changed(dir, ChangeAddEntry, "held", false)
	f.sink.takeNotes()
	d := f.caller(t, nfs("d", 1, entity.NewUser()))
	od := f.mustOpen(t, d, in(1), OpenRequest{Access: rd})
	if err := f.Revoke(ctx, d.Client); err != nil {
		t.Fatal(err)
	}
	f.sink.takeRevoked()

	moved := netip.MustParseAddr("192.0.2.9")
	if err := f.Recovery().DropShare(ctx, f.fs.Share.ID, moved); err != nil {
		t.Fatal(err)
	}
	told := map[entity.ClientID]bool{}
	for _, rv := range f.sink.takeRevoked() {
		if rv.Reason != RevokedShare || rv.Share != f.fs.Share.ID || rv.Addr != moved {
			t.Errorf("told %+v, want RevokedShare naming the share and the address", rv)
		}
		told[rv.Client] = true
	}
	if !told[a.Client] || !told[b.Client] || !told[c.Client] || len(told) != 3 {
		t.Errorf("told %v, want each holder once", told)
	}
	if errs := f.Recovery().TestState(ctx, a, []StateRef{oa.Ref}); !errors.Is(errs[0], ErrBadState) {
		t.Errorf("a reference of the dropped share: %v, want ErrBadState", errs[0])
	}
	if errs := f.Recovery().TestState(ctx, d, []StateRef{od.Ref}); !errors.Is(errs[0], ErrBadState) {
		t.Errorf("a revoked reference of the dropped share: %v, want ErrBadState", errs[0])
	}
	if st, err := f.Renew(ctx, d.Client); err != nil || st.AdminRevoked {
		t.Errorf("status after the drop %+v, %v; want no flag left to clear", st, err)
	}
	f.handWatched()
	if n, w := f.sink.takeNotes(), f.sink.takeWaits(); len(n) != 0 || len(w) != 0 {
		t.Errorf("the dropped share's held changes %+v and waiter %+v were reported", n, w)
	}
	if f.entryOf(held.ID) != nil || f.entryOf(dir.ID) != nil || f.entryOf(file.ID) != nil || f.entryOf(src.ID) != nil {
		t.Error("the dropped share's files still hold state")
	}
	if _, err := f.Opens().Close(ctx, b, refOf(other), Seqs{}); err != nil {
		t.Errorf("another share's open after the drop: %v", err)
	}
	f.sweep(ctx)
	f.Stop() // waits for the steps the sweep started
	ended := slices.ContainsFunc(f.sink.graces, func(g graceEnded) bool { return g.share == f.fs.Share.ID })
	if got := append(f.stored(t, file), f.stored(t, src)...); len(got) != 2 || ended {
		t.Errorf("stored after the drop %+v, graces ended %+v; want the persistent record and the clone's hold left to the store", got, f.sink.graces)
	}
}

// DropShare in grace ends the grace with the state: a reclaim there is
// ErrNoReclaim, as nothing is left to reclaim.
func TestDropShareInGrace(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfsAs("a", entity.RecoverStatus))
	file := in(1)
	f.mustOpen(t, a, file, OpenRequest{Access: rd, Owner: owner(a, "oo")})
	g := f.restart(t)
	g.renew(t, a)
	if err := g.Recovery().DropShare(ctx, f.fs.Share.ID, netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.open(a, file, OpenRequest{Access: rd, Owner: owner(a, "oo"), Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim in the dropped share: %v, want ErrNoReclaim", err)
	}
}

// A grace-end pass under way when DropShare runs stops at the dropped
// share: it closes none of its records, though their opens went, and raises
// no GraceEnded for it.
func TestDropShareStopsAGraceEndPass(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, smbMachine("pc-c"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	f.mustOpen(t, c, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	f.Stop()

	g := unstarted(t, f.fs, f.clock)
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	order := func(x entity.FileID) func() {
		if x == file.ID && armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return func() {}
	}
	if err := g.Start(ctx, g.sink, order); err != nil {
		t.Fatal(err)
	}
	for _, s := range testShards {
		if err := g.Recovery().ServeShard(ctx, f.fs.Share.ID, s, instances.Add(1)); err != nil {
			t.Fatal(err)
		}
	}
	g.clock.mu.Lock()
	g.clock.now += leasePeriod + time.Second
	g.clock.mu.Unlock()
	armed.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); g.closeUnreclaimed(ctx, g.sink) }()
	<-entered
	if err := g.Recovery().DropShare(ctx, f.fs.Share.ID, netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if got := g.stored(t, file); len(got) != 1 {
		t.Errorf("stored after DropShare %+v; want the persistent record left to the store", got)
	}
	if slices.ContainsFunc(g.sink.graces, func(x graceEnded) bool { return x.s == file.Shard }) {
		t.Errorf("GraceEnded raised for the dropped shard: %+v", g.sink.graces)
	}
}

// blockingRand is a Deps.Rand whose next read, once armed, waits until
// released.
type blockingRand struct {
	armed    atomic.Bool
	entered  chan struct{}
	released chan struct{}
	r        *seeded
}

// wait waits for a read to enter b, failing the test after 5 s.
func (b *blockingRand) wait(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no read of the random source within 5 s")
	}
}

func (b *blockingRand) Read(p []byte) (int, error) {
	if b.armed.CompareAndSwap(true, false) {
		close(b.entered)
		<-b.released
	}
	return b.r.Read(p)
}

// A lock admitted before DropShare whose state is minted after the drop
// listed the share's state is refused, holding nothing; afterwards nothing
// is granted on the share and a reclaim is ErrNoReclaim.
func TestDropShareRefusesARacingLock(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	held, file := in(1), in(1)
	f.mustLock(t, a, held, LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}})
	rnd := &blockingRand{entered: make(chan struct{}), released: make(chan struct{}), r: newSeeded(t.Name())}
	f.d.Rand = rnd
	rnd.armed.Store(true)
	locked := make(chan error)
	go func() {
		_, err := f.lock(a, file, LockReq{Owner: owner(a, "other"), Ranges: []LockRange{x(0, 10)}})
		locked <- err
	}()
	rnd.wait(t) // admitted, minting its lock state
	dropped := make(chan error)
	go func() { dropped <- f.Recovery().DropShare(ctx, f.fs.Share.ID, netip.Addr{}) }()
	for !f.droppedShard(file.Shard) {
		runtime.Gosched()
	}
	close(rnd.released)
	if err := <-locked; !errors.Is(err, ErrBadState) {
		t.Errorf("a lock minting after the drop: %v, want ErrBadState", err)
	}
	if err := <-dropped; err != nil {
		t.Fatal(err)
	}
	if f.entryOf(file.ID) != nil || f.entryOf(held.ID) != nil {
		t.Error("the dropped share's files still hold state")
	}
	if _, err := f.lock(a, file, LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}}); !errors.Is(err, ErrBadState) {
		t.Errorf("a lock on the dropped share: %v, want ErrBadState", err)
	}
	if _, err := f.lock(a, held, LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim on the dropped share: %v, want ErrNoReclaim", err)
	}
	if _, err := f.Locks().TestLock(ctx, a, file, owner(a, "lo"), br(0, 10), true); !errors.Is(err, ErrBadState) {
		t.Errorf("a test of a lock on the dropped share: %v, want ErrBadState", err)
	}
	// Served anew under its instance, as a move back serves it, its grace
	// takes reclaims again.
	f.cmu.Lock()
	inst := f.shards[file.Shard].instance
	f.cmu.Unlock()
	if err := f.Recovery().ServeShard(ctx, f.fs.Share.ID, file.Shard, inst); err != nil {
		t.Fatal(err)
	}
	f.mustLock(t, a, held, LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
}

// droppedShard reports whether DropShare has marked shard s dropped.
func (f *fixture) droppedShard(s ShardID) bool {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	return f.shards[s] != nil && f.shards[s].dropped
}

// A DropShare racing a serve of one of its share's shards, while the serve
// is writing the restart notice with the shard out of the served set, waits
// for the serve: the shard then comes out dropped, not served in grace.
func TestDropShareWaitsForAServe(t *testing.T) {
	fs, held := kvstoretest.NewHeldFS(t, memKV)
	f := reopen(t, fs, &fakeClock{})
	h := f.host(t, "h", 1) // a notice client in shard 1, so its serve raises the notice
	f.mustLock(t, h, in(1), LockReq{Owner: owner(h, "lo"), Ranges: []LockRange{x(0, 10)}})
	dropped := make(chan error, 1)
	serveErr, _ := held.Race(
		func() error { return f.Recovery().ServeShard(ctx, fs.Share.ID, 1, instances.Add(1)) },
		func() error {
			go func() { dropped <- f.Recovery().DropShare(ctx, fs.Share.ID, netip.Addr{}) }()
			select {
			case err := <-dropped:
				dropped <- err // it ran inside the serve
			case <-time.After(200 * time.Millisecond): // it waits for the serve
			}
			return nil
		},
	)
	if serveErr != nil {
		t.Fatal(serveErr)
	}
	if err := <-dropped; err != nil {
		t.Fatal(err)
	}
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	if _, err := f.open(a, in(1), OpenRequest{Access: rd}); !errors.Is(err, ErrBadState) {
		t.Errorf("an open in the shard once DropShare returned: %v, want ErrBadState", err)
	}
}

// A restart's serve of a shard leases each NFSv4 client the shard's records
// name: one silent for a lease period is expired, its record and its marks
// gone and ClientEnded raised once, and one renewing within it is kept.
func TestRestartLeasesListedClients(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfsAs("a", entity.RecoverStatus)), f.caller(t, nfsAs("b", entity.RecoverStatus))
	f.mustLock(t, a, in(5), LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, b, in(5), LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.pass(t, leasePeriod+sweepEvery, b.Client)
	g.sweep(ctx)
	g.sweep(ctx)
	if _, err := f.fs.Store.Clients().Get(ctx, a.Client); err == nil {
		t.Error("a client silent since the restart kept its record")
	}
	if got := g.sink.endedOf(a.Client); len(got) != 1 {
		t.Errorf("ClientEnded for the silent client %v, want once", got)
	}
	if _, err := f.fs.Store.Clients().Get(ctx, b.Client); err != nil || len(g.sink.endedOf(b.Client)) != 0 {
		t.Errorf("the renewing client: record %v, ended %v; want it kept", err, g.sink.endedOf(b.Client))
	}
}

// The serve of a shard leases no NLM host or SMB client it lists, and leaves
// a lease already held where it ends. The NLM and SMB half pins leaseEnd,
// which gives either no end, rather than the serve's own protocol check.
func TestServeLeasesOnlyUnleasedNFSv4Clients(t *testing.T) {
	f := newFixture(t)
	h, s := f.host(t, "h", 1), f.caller(t, smb("pc"))
	a := f.caller(t, nfsAs("a", entity.RecoverStatus))
	f.mustLock(t, h, in(5), LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	f.mustOpen(t, s, in(5), OpenRequest{Access: rd})
	f.mustLock(t, a, in(5), LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}})
	f.Stop()

	g := unstarted(t, f.fs, f.clock)
	g.start(t, nil)
	g.renew(t, a)
	g.clock.advance(leasePeriod / 2)
	if err := g.Recovery().ServeShard(ctx, f.fs.Share.ID, 5, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	g.clock.advance(leasePeriod / 2)
	g.sweep(ctx)
	if got := g.sink.endedOf(a.Client); len(got) != 1 {
		t.Errorf("a client leased before the serve, silent a period since: ended %v, want once", got)
	}
	g.pass(t, 3*leasePeriod)
	g.sweep(ctx)
	for _, c := range []Caller{h, s} {
		if _, err := f.fs.Store.Clients().Get(ctx, c.Client); err != nil || len(g.sink.endedOf(c.Client)) != 0 {
			t.Errorf("the NLM host or SMB client %d: record %v, ended %v; want it kept", c.Client, err, g.sink.endedOf(c.Client))
		}
	}
}

// Another principal's collision with the name of an NFSv4 client holding
// state, after a restart and before its shard is served, leases nothing: the
// client's lease starts at its shard's grace, so it is not expired first.
func TestCollisionBeforeServeLeavesAStatefulClient(t *testing.T) {
	for name, take := range map[string]func(f *fixture, c Client) error{
		"Propose": func(f *fixture, c Client) error { _, err := f.Propose(ctx, c, addr); return err },
		"Connect": func(f *fixture, c Client) error { _, err := f.Connect(ctx, c, nil); return err },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := f.caller(t, nfsAs("a", entity.RecoverStatus))
			f.mustLock(t, a, in(5), LockReq{Owner: owner(a, "lo"), Ranges: []LockRange{x(0, 10)}})
			f.Stop()
			g := unstarted(t, f.fs, f.clock)
			g.start(t, nil)
			_ = take(g, nfs("a", 2, entity.NewUser())) // refused: the name is a's
			g.clock.advance(leasePeriod + sweepEvery)
			g.sweep(ctx)
			if got := g.sink.endedOf(a.Client); len(got) != 0 {
				t.Errorf("a client whose shard is not served yet was expired by another principal's %s: ended %v", name, got)
			}
		})
	}
}

// A silent NFSv4 client that held nothing, so no serve listed it, frees its
// name to another principal after a restart as it does without one: the
// collision arms its lease, and the name lapses a lease period later.
func TestSilentClientsNameLapsesAfterARestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for name, take := range map[string]func(f *fixture, c Client) error{
			"Propose": func(f *fixture, c Client) error { _, err := f.Propose(ctx, c, addr); return err },
			"Connect": func(f *fixture, c Client) error { _, err := f.Connect(ctx, c, nil); return err },
		} {
			t.Run(fmt.Sprintf("%s restart=%v", name, restart), func(t *testing.T) {
				f := newFixture(t)
				f.caller(t, nfs("host-a", 1, entity.NewUser()))
				if restart {
					f = f.restart(t)
				}
				f.pass(t, 4*leasePeriod)
				other := nfs("host-a", 2, entity.NewUser())
				err := take(f, other)
				if restart {
					if !errors.Is(err, ErrClientInUse) {
						t.Fatalf("another principal, at the name's first collision since the restart: %v, want ErrClientInUse", err)
					}
					f.pass(t, leasePeriod+sweepEvery)
					err = take(f, other)
				}
				if err != nil {
					t.Errorf("another principal, a lease period after the name went silent: %v", err)
				}
			})
		}
	}
}

// An expiry whose record delete is refused releases nothing: another client
// is granted the lapsed client's lock only once its record is gone, so after
// a restart the lapsed client's reclaim is refused, and the lock's new
// holder reclaims it.
func TestRefusedExpiryReleasesNothing(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	x1, y := f.caller(t, nfsAs("x", entity.RecoverStatus)), f.caller(t, nfsAs("y", entity.RecoverStatus))
	file := in(5)
	f.mustLock(t, x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, y, in(5), LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}}) // y's shard mark
	// x lapses; pass fires no timer, so the sweep below is the only one
	f.pass(t, leasePeriod+time.Second, y.Client)

	st.fail.Store(true)
	f.sweep(ctx) // x's expiry is refused
	_, err := f.lock(y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}})
	st.fail.Store(false)
	if err != nil {
		f.sweep(ctx)
		f.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}})
	}

	g := f.restart(t)
	g.renew(t, y)
	if _, err := g.lock(x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); err == nil {
		t.Error("the lapsed client's reclaim after the restart was granted, want refused")
	}
	g.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
}

// A revoke whose record write is refused releases nothing, as an expiry's:
// after a restart the revoked client's reclaim is refused, and the lock's new
// holder reclaims it.
func TestRefusedRevokeReleasesNothing(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	x1, y := f.caller(t, nfsAs("x", entity.RecoverStatus)), f.caller(t, nfsAs("y", entity.RecoverStatus))
	file := in(5)
	f.mustLock(t, x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, y, in(5), LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}}) // y's shard mark

	st.fail.Store(true)
	if err := f.Revoke(ctx, x1.Client); !errors.Is(err, errWrite) {
		t.Fatalf("a revoke over a refusing store: %v, want its error", err)
	}
	_, err := f.lock(y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}})
	st.fail.Store(false)
	if err != nil {
		if err := f.Revoke(ctx, x1.Client); err != nil {
			t.Fatal(err)
		}
		f.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}})
	}

	g := f.restart(t)
	g.renew(t, x1, y)
	if _, err := g.lock(x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("the revoked client's reclaim after the restart: %v, want ErrNoReclaim", err)
	}
	g.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
}

// A revoke retried after its record delete landed but was answered an error
// finds no record and still releases what the client holds.
func TestRevokeRetriedAfterLandedDeleteReleases(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	h1, h2 := f.host(t, "h1", 1), f.host(t, "h2", 1)
	file := in(5)
	f.mustLock(t, h1, file, LockReq{Owner: owner(h1, "p"), Ranges: []LockRange{x(0, 10)}})

	st.lose.Store(true)
	if err := f.Revoke(ctx, h1.Client); !errors.Is(err, errWrite) {
		t.Fatalf("a revoke whose delete is answered an error: %v, want that error", err)
	}
	st.lose.Store(false)
	if err := f.Revoke(ctx, h1.Client); !errors.Is(err, ErrStaleClient) {
		t.Errorf("the retried revoke: %v, want ErrStaleClient", err)
	}
	if _, err := f.lock(h2, file, LockReq{Owner: owner(h2, "p"), Ranges: []LockRange{x(0, 10)}}); err != nil {
		t.Errorf("another host's lock after the retried revoke: %v, want granted", err)
	}
}
